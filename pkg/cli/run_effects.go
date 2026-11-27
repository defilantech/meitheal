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
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// AssistedBy is the attribution line every finalized PR ends with, used
	// verbatim (for example "Assisted-by: Meitheal; <coder> wrote the
	// change, <reviewer> reviewed it"); "; gates: " and the gate commands
	// that ran are appended. Empty uses a neutral default.
	AssistedBy string `json:"assistedBy,omitempty"`
}

// LoadWorkloadTemplate reads a WorkloadTemplate from a YAML file with the
// keys spec, gpuGate, gpuNode and assistedBy. Unknown keys are an error: a misspelt
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
	if t.Spec.GateProfile == nil {
		// Without it the CPU gate resolves to the Go preset, which is not a
		// gate for anything this loop runs.
		return WorkloadTemplate{}, fmt.Errorf("workload template %s: spec.gateProfile is required", path)
	}
	if (t.GPUGate == nil) != (t.GPUNode == "") {
		return WorkloadTemplate{}, fmt.Errorf("workload template %s: gpuGate and gpuNode go together", path)
	}
	// The busy check looks at gpuNode; a placement pinning the gate to a
	// different host would check one box and run on another.
	if t.GPUGate != nil && t.GPUGate.Placement != nil {
		if h, ok := t.GPUGate.Placement.NodeSelector[hostnameLabel]; ok && h != t.GPUNode {
			return WorkloadTemplate{}, fmt.Errorf(
				"workload template %s: gpuGate is placed on %s=%q but gpuNode is %q", path, hostnameLabel, h, t.GPUNode)
		}
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
	// GPUPaths are the planner's --gpu-paths prefixes. A cpu-lane branch
	// that changes a file under one fails Verify with GPUPathEvidence and
	// is verified as gpu-lane from then on. Empty: no check. Cleaned like
	// the planner's.
	GPUPaths []string
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
	o.GPUPaths = cleanPrefixes(o.GPUPaths)
	return o
}

func validFactor(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0
}

// hostnameLabel is the well-known node label a placement pins a host with.
const hostnameLabel = "kubernetes.io/hostname"

// evidenceTailBytes is how much of a failed gate's evidence the feedback
// pass hands the coder: the end of a log is where the failure is.
const evidenceTailBytes = 4000

// GPUBusyEvidence is the verify evidence for a GPU node that stayed busy.
// The driver matches it exactly to park instead of spending the feedback
// pass on it.
const GPUBusyEvidence = "gpu-box-busy"

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
	// gpuOverride holds the Workloads of cpu-lane items Verify moved to the
	// gpu lane (see GPUPathEvidence), also recorded on each Workload made
	// from then on (runLaneAnnotation).
	gpuOverride map[string]bool
}

// NewClusterEffects returns the Effects the run command drives: Workloads
// in namespace ns built from t, gates run by g, issue and PR writes via f.
func NewClusterEffects(
	c client.Client, ns string, f RunForge, t WorkloadTemplate, g GateRunner, o ClusterEffectsOptions,
) Effects {
	return &clusterEffects{
		c: c, ns: ns, forge: f, tpl: t, gate: g,
		o:           o.withDefaults(),
		http:        &http.Client{Timeout: notifyTimeout},
		items:       map[string]QueueItem{},
		gpuOverride: map[string]bool{},
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
		// Concerns is kept raw: it can come from the model, and concernOf
		// reads it fail closed instead of letting a bad shape fail the
		// whole decode.
		Concerns       json.RawMessage
		PullRequestURL string
		ModelExtra     map[string]any
	}
}

// parseTaskResult decodes each field of status.result.extra on its own, so a
// value of the wrong type in one (the model writes into extra) cannot hide
// the others: the PR URL is still read next to an unreadable concern.
func parseTaskResult(t *meithealv1alpha1.AgenticTask) (runTaskResult, bool) {
	var r runTaskResult
	if t.Status.Result == nil || len(t.Status.Result.Raw) == 0 {
		return r, false
	}
	var top struct {
		Extra map[string]json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(t.Status.Result.Raw, &top); err != nil {
		return r, false
	}
	r.Extra.Concerns = top.Extra["concerns"]
	_ = json.Unmarshal(top.Extra["pullRequestURL"], &r.Extra.PullRequestURL)
	_ = json.Unmarshal(top.Extra["modelExtra"], &r.Extra.ModelExtra)
	return r, true
}

// concernNoText stands in for a concern the loop cannot read the text of.
const concernNoText = "a concern with no text"

// concernOf reads extra.concerns fail closed: absent, null or an empty list
// is no concern; a list returns its first concern's text; anything else (a
// string, an object, a list entry that is not an object or has no text) is
// still a concern, reported as concernNoText. A concern parks the item, so
// a shape the loop does not expect must never read as none.
func concernOf(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return concernNoText
	}
	if len(list) == 0 {
		return ""
	}
	for _, el := range list {
		var c struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(el, &c); err == nil && strings.TrimSpace(c.Text) != "" {
			return c.Text
		}
	}
	return concernNoText
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
			res.Concern = concernOf(r.Extra.Concerns)
		}
		if res.PRURL == "" {
			res.PRURL = r.Extra.PullRequestURL
		}
	}
	return res, claimed
}

// lastIsRejectedReview reports whether the run ended on a review requesting
// changes: the newest review task (by creation time, then name) has verdict
// NO-GO, and no other task was created after it. A revision coder (or its
// verify step) is created after the review it answers, so a revision that
// failed is not the review's failure. Tasks emitted together with the review
// (the code and verify steps of the same round) share its creation time and
// do not count as after it.
func lastIsRejectedReview(tasks []meithealv1alpha1.AgenticTask) bool {
	var last *meithealv1alpha1.AgenticTask
	for i := range tasks {
		t := &tasks[i]
		if t.Spec.Kind != meithealv1alpha1.AgenticTaskKindReview {
			continue
		}
		if last == nil || last.CreationTimestamp.Before(&t.CreationTimestamp) ||
			(last.CreationTimestamp.Equal(&t.CreationTimestamp) && last.Name < t.Name) {
			last = t
		}
	}
	if last == nil || last.Status.Verdict != meithealv1alpha1.AgenticTaskVerdictNoGo {
		return false
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Spec.Kind != meithealv1alpha1.AgenticTaskKindReview && last.CreationTimestamp.Before(&t.CreationTimestamp) {
			return false
		}
	}
	return true
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
			res.ReviewRejected = lastIsRejectedReview(tasks)
			res.FailureReason, res.FailureEvidence = explainFailure(tasks, maxReviewIterations(w))
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
//
// A cpu-lane branch that changes a file under GPUPaths is not gated at all:
// it is reported not clean with GPUPathEvidence and moved to the gpu lane,
// so the feedback pass is verified by both gates. Listing the changed files
// fails closed.
func (e *clusterEffects) Verify(ctx context.Context, workload, branch string) (bool, string, error) {
	item, ok := e.itemFor(workload)
	if !ok {
		return false, "", fmt.Errorf("verify: workload %s was not dispatched by this run", workload)
	}
	repo := e.repoFor(item)
	touches, err := e.touchesGPUPaths(ctx, workload, item, branch)
	if err != nil {
		return false, "", err
	}
	if touches {
		e.setGPUOverride(workload)
		return false, GPUPathEvidence, nil
	}
	var cpu meithealv1alpha1.GateProfile
	if e.tpl.Spec.GateProfile != nil {
		cpu = *e.tpl.Spec.GateProfile
	}
	clean, log, err := e.gate.Run(ctx, repo, branch, cpu)
	if err != nil || !clean || !e.gpuLane(workload, item) {
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
			return false, GPUBusyEvidence, nil
		}
		if err := sleepCtx(ctx, e.o.GPURetry); err != nil {
			return false, "", err
		}
	}
	return e.gate.Run(ctx, repo, branch, *e.tpl.GPUGate)
}
