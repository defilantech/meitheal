/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// IssueClosedReason is the preflight skip reason for a closed issue. A skip
// with exactly this reason releases the items that run after it, like a
// finished one: the work it stood for is settled.
const IssueClosedReason = "issue is closed"

// RunLostReason is the park reason for an issue whose own task branch exists
// with no Workload of the loop's behind it.
const RunLostReason = "the loop lost track of this issue's run"

// ErrRunLost is what Preflight returns when the issue's own task branch
// exists but its Workload does not: a stop during a feedback pass's delete
// wait, a recreate that failed, a Workload deleted by hand. Resuming has
// nothing to watch and dispatching would start over on top of the branch, so
// the driver parks it escalate instead (Facts.RunLost); a maintainer's
// requeue then starts a new round on the branch.
var ErrRunLost = errors.New(RunLostReason)

// GPUPathEvidence is the verify evidence for a cpu-lane branch that changes a
// file under one of the GPU paths. Verify returns it without running a gate
// and from then on runs the item as gpu-lane, so the feedback pass is
// checked by the CPU gate and then the GPU gate.
const GPUPathEvidence = "this change touches GPU code and needs the GPU gate"

// runLaneAnnotation records on a Workload that Verify moved its item to the
// gpu lane, so a restarted loop keeps running the GPU gate on it.
const runLaneAnnotation = "meitheal.dev/run-lane"

// verifyBaseBranch is the base a branch's changed files are listed against:
// the base an issue-fix coder branches from (AgenticTask baseBranch's
// default).
const verifyBaseBranch = "main"

// ownedRun reports whether w is this item's run: it carries the issue and
// works in the item's repository. A Workload that carries the issue in
// another repository is never adopted, watched or deleted: that is an error
// naming both repositories. nil, or one without the issue, is not the item's
// (false, nil).
func (e *clusterEffects) ownedRun(w *meithealv1alpha1.Workload, item QueueItem) (bool, error) {
	if w == nil || !slices.Contains(w.Spec.Issues, item.Issue) {
		return false, nil
	}
	if want := e.repoFor(item); w.Spec.Repo != want {
		return false, fmt.Errorf("workload %s runs issue %d in %q, not in %q; leaving it alone",
			w.Name, item.Issue, w.Spec.Repo, want)
	}
	return true, nil
}

// Preflight decides whether an item is dispatched, in this order:
//
//   - A closed issue is skipped with IssueClosedReason.
//   - The item's own Workload (reserved name, the issue, the item's
//     repository) means the loop was restarted mid-run, or a manual run left
//     it: the branch and PR preflight would see are that Workload's, so the
//     item resumes.
//   - The item's own task branch without its Workload is ErrRunLost, for the
//     driver to park.
//   - Otherwise the package-level Preflight probes for an open PR or branch
//     someone else made.
//
// Every read fails closed: an error is returned, never taken as "nothing
// found".
func (e *clusterEffects) Preflight(ctx context.Context, item QueueItem, branch string) (string, error) {
	if item.Repo == "" || branch == "" {
		// The package-level Preflight refuses these before any probe.
		return Preflight(ctx, e.forge, item, branch)
	}
	iss, err := e.forge.FetchIssue(ctx, item.Repo, item.Issue)
	if err != nil {
		return "", fmt.Errorf("preflight: read issue %d: %w", item.Issue, err)
	}
	if strings.EqualFold(iss.State, "closed") {
		return IssueClosedReason, nil
	}
	name := plannedWorkloadName(item.Issue)
	w, err := e.existing(ctx, name)
	if err != nil {
		return "", err
	}
	own, err := e.ownedRun(w, item)
	if err != nil {
		return "", fmt.Errorf("preflight: %w", err)
	}
	if own {
		return "", nil
	}
	lost, err := e.forge.BranchExists(ctx, item.Repo, taskBranch(name, item.Issue))
	if err != nil {
		return "", fmt.Errorf("preflight: branch probe for issue %d: %w", item.Issue, err)
	}
	if lost {
		return "", ErrRunLost
	}
	return Preflight(ctx, e.forge, item, branch)
}

// touchesPrefix reports whether any file starts with one of the prefixes,
// the planner's lane rule (prefixes cleaned by cleanPrefixes).
func touchesPrefix(files, prefixes []string) bool {
	for _, f := range files {
		for _, p := range prefixes {
			if strings.HasPrefix(f, p) {
				return true
			}
		}
	}
	return false
}

// gpuLane reports whether Verify runs the item's Workload as gpu-lane: the
// queue put it there, or Verify moved it there after its branch touched a
// GPU path.
func (e *clusterEffects) gpuLane(workload string, item QueueItem) bool {
	if item.Lane == laneGPU {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gpuOverride[workload]
}

func (e *clusterEffects) setGPUOverride(workload string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gpuOverride[workload] = true
}

// adoptLane carries a Workload's recorded lane override into this run, so a
// resumed or recreated Workload keeps the GPU gate Verify gave it.
func (e *clusterEffects) adoptLane(w *meithealv1alpha1.Workload) {
	if w != nil && w.Annotations[runLaneAnnotation] == laneGPU {
		e.setGPUOverride(w.Name)
	}
}

// touchesGPUPaths lists the branch's changed files and reports whether one is
// under a GPU path. A cpu-lane item only, and only when GPU paths are set; a
// failed listing is an error, never "nothing touched".
func (e *clusterEffects) touchesGPUPaths(ctx context.Context, workload string, item QueueItem, branch string) (
	bool, error,
) {
	if len(e.o.GPUPaths) == 0 || e.gpuLane(workload, item) {
		return false, nil
	}
	files, err := e.forge.ChangedFiles(ctx, e.repoFor(item), verifyBaseBranch, branch)
	if err != nil {
		return false, fmt.Errorf("verify issue %d: list the files %s changes: %w", item.Issue, branch, err)
	}
	return touchesPrefix(files, e.o.GPUPaths), nil
}
