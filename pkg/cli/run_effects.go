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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// WorkloadTemplate is everything a run's Workloads share. Dispatch copies
// Spec and fills in the per-item fields (intent, issue, repo); GPUGate and
// GPUNode are the run loop's second, GPU-placed gate.
type WorkloadTemplate struct {
	// Spec is the Workload spec every dispatch starts from: repo, agents,
	// gateProfile (the CPU gate), maxReviewIterations, mcpEnabled.
	Spec meithealv1alpha1.WorkloadSpec `json:"spec"`
	// GPUGate is the gate a gpu-lane item must also pass. Nil: no GPU lane.
	GPUGate *meithealv1alpha1.GateProfile `json:"gpuGate,omitempty"`
	// GPUNode is the node the GPU gate runs on, checked for a serving
	// model before each GPU gate run.
	GPUNode string `json:"gpuNode,omitempty"`
}

// LoadWorkloadTemplate reads a WorkloadTemplate from a YAML file with the
// keys spec, gpuGate and gpuNode. Unknown keys are an error: a misspelt
// gpuGate would otherwise drop the GPU gate without a word.
func LoadWorkloadTemplate(path string) (WorkloadTemplate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return WorkloadTemplate{}, fmt.Errorf("read workload template: %w", err)
	}
	var t WorkloadTemplate
	if err := sigsyaml.UnmarshalStrict(b, &t); err != nil {
		return WorkloadTemplate{}, fmt.Errorf("parse workload template %s: %w", path, err)
	}
	if (t.GPUGate == nil) != (t.GPUNode == "") {
		return WorkloadTemplate{}, fmt.Errorf("workload template %s: gpuGate and gpuNode go together", path)
	}
	return t, nil
}

// ClusterEffectsOptions tunes the production Effects. Zero values take the
// defaults noted on each field.
type ClusterEffectsOptions struct {
	// Poll is how often Watch and the delete wait re-read the cluster. 30s.
	Poll time.Duration
	// GPURetry is how long Verify waits between GPU-busy checks. 15m.
	GPURetry time.Duration
	// GPUGiveUp is how long the GPU node may stay busy before Verify reports
	// the item not clean with evidence "gpu-box-busy". 12h.
	GPUGiveUp time.Duration
	// StallFactor is used when Watch is handed a factor that is not a
	// positive number. DefaultStallFactor.
	StallFactor float64
	// Now is the clock. time.Now.
	Now func() time.Time
	// NotifyURL, when set, receives a JSON POST for every park.
	NotifyURL string
	// ClaimTimeout bounds how long Watch waits for the coder task to be
	// claimed at all. Unclaimed time never counts toward a stall, so this is
	// what stops a lane hanging on a task no agent picks up. 6h.
	ClaimTimeout time.Duration
}

func (o ClusterEffectsOptions) withDefaults() ClusterEffectsOptions {
	if o.Poll <= 0 {
		o.Poll = 30 * time.Second
	}
	if o.GPURetry <= 0 {
		o.GPURetry = 15 * time.Minute
	}
	if o.GPUGiveUp <= 0 {
		o.GPUGiveUp = 12 * time.Hour
	}
	if !validFactor(o.StallFactor) {
		o.StallFactor = DefaultStallFactor
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ClaimTimeout <= 0 {
		o.ClaimTimeout = 6 * time.Hour
	}
	return o
}

func validFactor(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0
}

// evidenceTailBytes is how much of a failed gate's evidence the feedback
// pass hands the coder: the end of a log is where the failure is.
const evidenceTailBytes = 4000

// gpuBusyEvidence is the verify evidence for a GPU node that stayed busy.
const gpuBusyEvidence = "gpu-box-busy"

// notifyTimeout bounds the park webhook POST.
const notifyTimeout = 10 * time.Second

// clusterEffects is the production Effects: Workloads through the
// controller-runtime client, gates through a GateRunner, and the issue and
// PR through a RunForge.
type clusterEffects struct {
	c     client.Client
	ns    string
	forge RunForge
	tpl   WorkloadTemplate
	gate  GateRunner
	o     ClusterEffectsOptions
	http  *http.Client

	mu sync.Mutex
	// items maps a Workload this run dispatched to its queue item, which
	// carries the lane and repo Verify, Finalize and Notify need. The
	// driver always passes through Dispatch (resuming or creating) before
	// any of them, including after a restart.
	items map[string]QueueItem
}

// NewClusterEffects returns the Effects the run command drives: Workloads
// in namespace ns built from t, gates run by g, issue and PR writes via f.
func NewClusterEffects(
	c client.Client, ns string, f RunForge, t WorkloadTemplate, g GateRunner, o ClusterEffectsOptions,
) Effects {
	return &clusterEffects{
		c: c, ns: ns, forge: f, tpl: t, gate: g,
		o:     o.withDefaults(),
		http:  &http.Client{Timeout: notifyTimeout},
		items: map[string]QueueItem{},
	}
}

func (e *clusterEffects) remember(workload string, item QueueItem) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.items[workload] = item
}

func (e *clusterEffects) itemFor(workload string) (QueueItem, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	it, ok := e.items[workload]
	return it, ok
}

func (e *clusterEffects) repoFor(item QueueItem) string {
	if item.Repo != "" {
		return item.Repo
	}
	return e.tpl.Spec.Repo
}

func (e *clusterEffects) key(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: e.ns, Name: name}
}

// existing returns the Workload named name, or nil when there is none.
func (e *clusterEffects) existing(ctx context.Context, name string) (*meithealv1alpha1.Workload, error) {
	var w meithealv1alpha1.Workload
	if err := e.c.Get(ctx, e.key(name), &w); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get workload %s: %w", name, err)
	}
	return &w, nil
}

// Preflight is the package-level Preflight, except when this item's reserved
// Workload is already in the cluster: then the loop was restarted mid-run (or
// a manual run left it), the branch and PR preflight would see are that
// Workload's own, and the item resumes rather than being skipped as done.
func (e *clusterEffects) Preflight(ctx context.Context, item QueueItem, branch string) (string, error) {
	w, err := e.existing(ctx, plannedWorkloadName(item.Issue))
	if err != nil {
		return "", err
	}
	if w != nil && slices.Contains(w.Spec.Issues, item.Issue) {
		return "", nil
	}
	return Preflight(ctx, e.forge, item, branch)
}

func (e *clusterEffects) newWorkload(
	name string, item QueueItem, intent string, revise bool,
) *meithealv1alpha1.Workload {
	spec := e.tpl.Spec.DeepCopy()
	spec.Intent = intent
	spec.Issues = []int32{item.Issue}
	spec.Repo = e.repoFor(item)
	spec.ReviseExistingBranch = revise
	return &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec:       *spec,
	}
}

// Dispatch creates the item's Workload under its reserved name, or resumes
// the one already there. A resumed Workload is never written to: not
// updated, not deleted, not recreated.
func (e *clusterEffects) Dispatch(ctx context.Context, item QueueItem, intent string) (string, error) {
	name := plannedWorkloadName(item.Issue)
	w, err := e.existing(ctx, name)
	if err != nil {
		return "", err
	}
	if w != nil && w.DeletionTimestamp == nil {
		// Resuming a Workload that is not running this issue would watch,
		// verify and eventually feed back (delete and recreate) somebody
		// else's run. Refuse instead.
		if !slices.Contains(w.Spec.Issues, item.Issue) {
			return "", fmt.Errorf("workload %s exists but is not an issue-batch run for issue %d; leaving it alone",
				name, item.Issue)
		}
		e.remember(name, item)
		return name, nil
	}
	if w != nil {
		// Still going away after a Kill; a resume would watch a corpse.
		if err := e.waitGone(ctx, name); err != nil {
			return "", err
		}
	}
	if err := e.c.Create(ctx, e.newWorkload(name, item, intent, false)); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create workload %s: %w", name, err)
		}
		// Lost a race with another creator: that is a resume too.
	}
	e.remember(name, item)
	return name, nil
}

// lastBytes returns at most the last n bytes of s, starting on a rune
// boundary so the coder is never handed a broken character.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// Redispatch runs the one feedback pass: the Workload is deleted, waited
// out, and recreated under the same name with ReviseExistingBranch, so the
// coder resumes the branch it pushed (and its draft PR) instead of starting
// over. Deleting a Workload removes its AgenticTasks and leaves the pushed
// branch alone.
func (e *clusterEffects) Redispatch(ctx context.Context, item QueueItem, workload, intent, evidence string) error {
	e.remember(workload, item)
	revised := intent +
		"\n\nThe previous attempt failed the independent gate. Fix this and nothing else:\n" +
		lastBytes(evidence, evidenceTailBytes)
	if err := e.deleteWorkload(ctx, workload); err != nil {
		return err
	}
	if err := e.waitGone(ctx, workload); err != nil {
		return err
	}
	if err := e.c.Create(ctx, e.newWorkload(workload, item, revised, true)); err != nil {
		return fmt.Errorf("recreate workload %s: %w", workload, err)
	}
	return nil
}

// deleteWorkload deletes in the foreground: the Workload stays visible until
// its AgenticTasks are gone, so waiting for it to vanish also waits out its
// children. A recreated Workload would otherwise render task names its
// predecessor's tasks still hold.
func (e *clusterEffects) deleteWorkload(ctx context.Context, name string) error {
	w := &meithealv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns}}
	if err := e.c.Delete(ctx, w, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("delete workload %s: %w", name, err)
	}
	return nil
}

// waitGone polls until the Workload is gone, bounded by ctx.
func (e *clusterEffects) waitGone(ctx context.Context, name string) error {
	for {
		w, err := e.existing(ctx, name)
		if err != nil {
			return err
		}
		if w == nil {
			return nil
		}
		if err := sleepCtx(ctx, e.o.Poll); err != nil {
			return fmt.Errorf("waiting for workload %s to be deleted: %w", name, err)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Kill stops a stalled Workload by deleting it. The branch it pushed, if
// any, stays.
func (e *clusterEffects) Kill(ctx context.Context, workload string) error {
	return e.deleteWorkload(ctx, workload)
}

// runTaskResult is the slice of an AgenticTask's status.result the run loop
// reads.
type runTaskResult struct {
	Extra struct {
		Concerns []struct {
			Kind  string   `json:"kind"`
			Text  string   `json:"text"`
			Files []string `json:"files"`
		} `json:"concerns"`
		PullRequestURL string         `json:"pullRequestURL"`
		ModelExtra     map[string]any `json:"modelExtra"`
	} `json:"extra"`
}

func parseTaskResult(t *meithealv1alpha1.AgenticTask) (runTaskResult, bool) {
	var r runTaskResult
	if t.Status.Result == nil || len(t.Status.Result.Raw) == 0 {
		return r, false
	}
	if err := json.Unmarshal(t.Status.Result.Raw, &r); err != nil {
		return r, false
	}
	return r, true
}

// workloadTasks lists the Workload's AgenticTasks in name order, so "the
// first concern" means the same task on every poll.
func (e *clusterEffects) workloadTasks(ctx context.Context, workload string) ([]meithealv1alpha1.AgenticTask, error) {
	var list meithealv1alpha1.AgenticTaskList
	if err := e.c.List(ctx, &list, client.InNamespace(e.ns),
		client.MatchingLabels{"meitheal.dev/workload": workload}); err != nil {
		return nil, fmt.Errorf("list tasks of workload %s: %w", workload, err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

// observe folds the tasks into a WatchResult (without Stalled) and returns
// when the coder first claimed its task, nil when no coder task is claimed.
// A revision coder claims later than the first, so the earliest claim is the
// one the stall clock starts from.
func observe(tasks []meithealv1alpha1.AgenticTask) (WatchResult, *time.Time) {
	var res WatchResult
	var claimed *time.Time
	for i := range tasks {
		t := &tasks[i]
		if t.Spec.Kind == meithealv1alpha1.AgenticTaskKindIssueFix {
			if t.Status.ClaimedAt != nil {
				if c := t.Status.ClaimedAt.Time; claimed == nil || c.Before(*claimed) {
					claimed = &c
				}
			}
			if t.Status.Branch != "" || t.Status.CommitSHA != "" {
				res.BranchPushed = true
			}
		}
		r, ok := parseTaskResult(t)
		if !ok {
			continue
		}
		if res.Concern == "" {
			for _, c := range r.Extra.Concerns {
				if c.Text != "" {
					res.Concern = c.Text
					break
				}
			}
		}
		if res.PRURL == "" {
			res.PRURL = r.Extra.PullRequestURL
		}
	}
	return res, claimed
}

// Watch polls the Workload until it is terminal, the coder stalls, or the
// coder goes unclaimed for ClaimTimeout. The stall clock starts at the coder
// task's status.claimedAt, so time spent queued behind other work never
// counts against the coder.
func (e *clusterEffects) Watch(
	ctx context.Context, workload string, baseline time.Duration, factor float64,
) (WatchResult, error) {
	if !validFactor(factor) {
		factor = e.o.StallFactor
	}
	var start time.Time
	for first := true; ; first = false {
		w, err := e.existing(ctx, workload)
		if err != nil {
			return WatchResult{}, err
		}
		if w == nil {
			return WatchResult{}, fmt.Errorf("watch: workload %s does not exist", workload)
		}
		tasks, err := e.workloadTasks(ctx, workload)
		if err != nil {
			return WatchResult{}, err
		}
		res, claimed := observe(tasks)
		now := e.o.Now()
		if first {
			start = now
		}
		switch w.Status.Phase {
		case meithealv1alpha1.WorkloadPhaseCompleted:
			return res, nil
		case meithealv1alpha1.WorkloadPhaseFailed:
			res.WorkloadFailed = true
			return res, nil
		}
		if claimed != nil {
			res.Stalled = IsStalled(StallInput{
				Elapsed:      now.Sub(*claimed),
				Baseline:     baseline,
				BranchPushed: res.BranchPushed,
			}, factor)
			if res.Stalled {
				return res, nil
			}
		} else if now.Sub(start) >= e.o.ClaimTimeout {
			return res, fmt.Errorf("watch: no coder task of workload %s was claimed within %s",
				workload, e.o.ClaimTimeout)
		}
		if err := sleepCtx(ctx, e.o.Poll); err != nil {
			return res, err
		}
	}
}

// Verify runs the CPU gate and, for a gpu-lane item, then the GPU gate on
// GPUNode. A dirty CPU gate returns its log without spending GPU time. A GPU
// node that stays busy for GPUGiveUp is reported as not clean with evidence
// "gpu-box-busy".
func (e *clusterEffects) Verify(ctx context.Context, workload, branch string) (bool, string, error) {
	item, ok := e.itemFor(workload)
	if !ok {
		return false, "", fmt.Errorf("verify: workload %s was not dispatched by this run", workload)
	}
	repo := e.repoFor(item)
	var cpu meithealv1alpha1.GateProfile
	if e.tpl.Spec.GateProfile != nil {
		cpu = *e.tpl.Spec.GateProfile
	}
	clean, log, err := e.gate.Run(ctx, repo, branch, cpu)
	if err != nil || !clean || item.Lane != "gpu" {
		return clean, log, err
	}
	if e.tpl.GPUGate == nil {
		return false, "", fmt.Errorf("verify: issue %d is on the gpu lane but the template has no gpuGate", item.Issue)
	}
	waitStart := e.o.Now()
	for {
		err := gpuBusy(ctx, e.c, e.tpl.GPUNode)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrGPUBusy) {
			return false, "", err
		}
		if e.o.Now().Sub(waitStart) >= e.o.GPUGiveUp {
			return false, gpuBusyEvidence, nil
		}
		if err := sleepCtx(ctx, e.o.GPURetry); err != nil {
			return false, "", err
		}
	}
	return e.gate.Run(ctx, repo, branch, *e.tpl.GPUGate)
}
