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
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

const otherRepo = "example/other"

// otherRepoWorkload is the reserved Workload running the issue, but in
// another repository.
func otherRepoWorkload() *meithealv1alpha1.Workload {
	return &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: plannedWorkloadName(effIssue), Namespace: effNS},
		Spec:       meithealv1alpha1.WorkloadSpec{Intent: "elsewhere", Repo: otherRepo, Issues: []int32{effIssue}},
	}
}

// Every place the loop adopts or acts on the reserved Workload refuses one
// that runs the issue in another repository: an error naming both
// repositories, and the Workload is neither deleted nor rewritten.
func TestWorkloadInAnotherRepoIsNeverAdopted(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	acts := map[string]func(Effects) error{
		"Preflight": func(e Effects) error {
			_, err := e.Preflight(context.Background(), effItem("cpu"), taskBranch(name, effIssue))
			return err
		},
		"Dispatch": func(e Effects) error {
			_, _, err := e.Dispatch(context.Background(), effItem("cpu"), "fix it")
			return err
		},
		"HasRound": func(e Effects) error {
			_, err := e.HasRound(context.Background(), effItem("cpu"))
			return err
		},
		"Restart": func(e Effects) error {
			_, _, err := e.Restart(context.Background(), effItem("cpu"), "fix it")
			return err
		},
		"Redispatch": func(e Effects) error {
			return e.Redispatch(context.Background(), effItem("cpu"), name, "fix it", "ev")
		},
	}
	for label, act := range acts {
		t.Run(label, func(t *testing.T) {
			writes := 0
			funcs := &interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
					writes++
					return c.Delete(ctx, o, opts...)
				},
				Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
					writes++
					return c.Create(ctx, o, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, o, opts...)
				},
			}
			c := newEffectsClient(t, funcs, otherRepoWorkload())
			e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
			err := act(e)
			if err == nil {
				t.Fatal("want an error for a Workload in another repository")
			}
			if !strings.Contains(err.Error(), otherRepo) || !strings.Contains(err.Error(), effRepo) {
				t.Errorf("err = %v, want it to name %q and %q", err, otherRepo, effRepo)
			}
			if writes != 0 {
				t.Errorf("%d write(s) to the cluster, want none", writes)
			}
			if got := getWorkload(t, c, name); got.Spec.Repo != otherRepo || got.Spec.Intent != "elsewhere" {
				t.Errorf("the other repository's Workload changed: %+v", got.Spec)
			}
		})
	}
}

// A cpu-lane branch that changes a file under a GPU path is not gated: it is
// not clean with GPUPathEvidence, and from then on the item is verified as
// gpu-lane, so the feedback pass runs the CPU gate and then the GPU gate.
// The feedback Workload records the lane, so a restarted loop keeps it.
func TestVerify_CPULaneTouchingGPUPathNeedsTheGPUGate(t *testing.T) {
	c := newEffectsClient(t, nil)
	g := &fakeGateRunner{}
	f := newFakeRunForge()
	tpl := testTemplate()
	o := fastOpts()
	o.GPUPaths = []string{"crates/gpu-kernels/", "./crates/cuda"}
	e := NewClusterEffects(c, effNS, f, tpl, g, o)
	name := dispatched(t, e, "cpu")
	branch := taskBranch(name, effIssue)
	f.changed[branch] = []string{"README.md", "crates/gpu-kernels/src/matmul.rs"}

	clean, ev, err := e.Verify(context.Background(), name, branch)
	if err != nil || clean || ev != GPUPathEvidence {
		t.Fatalf("first Verify = %v, %q, %v; want not clean with %q", clean, ev, err, GPUPathEvidence)
	}
	if len(g.calls) != 0 {
		t.Fatalf("gate runs = %d before the lane is settled, want 0", len(g.calls))
	}
	// The driver's feedback pass.
	if err := e.Redispatch(context.Background(), effItem("cpu"), name, "fix it", ev); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}
	if got := getWorkload(t, c, name).Annotations[runLaneAnnotation]; got != laneGPU {
		t.Errorf("feedback Workload lane annotation = %q, want %q", got, laneGPU)
	}
	clean, _, err = e.Verify(context.Background(), name, branch)
	if err != nil || !clean {
		t.Fatalf("second Verify = %v, %v; want clean", clean, err)
	}
	if len(g.calls) != 2 || !reflect.DeepEqual(g.calls[0].profile, *tpl.Spec.GateProfile) ||
		!reflect.DeepEqual(g.calls[1].profile, *tpl.GPUGate) {
		t.Fatalf("second Verify gate runs = %+v, want the CPU gate then the GPU gate", g.calls)
	}
	if f.compares != 1 {
		t.Errorf("changed files listed %d times, want once (the lane is settled after the first)", f.compares)
	}
	if gates := e.(*clusterEffects).gatesFor(effItem("cpu"), name); len(gates) != 5 {
		t.Errorf("PR body gates = %v, want the CPU gate's four commands and the GPU gate's one", gates)
	}

	// A restarted loop resumes the feedback Workload and keeps the lane.
	e2 := NewClusterEffects(c, effNS, f, tpl, &fakeGateRunner{}, o)
	if _, attempt, err := e2.Dispatch(context.Background(), effItem("cpu"), "fix it"); err != nil || attempt != 2 {
		t.Fatalf("resume = %d, %v; want attempt 2", attempt, err)
	}
	if !e2.(*clusterEffects).gpuLane(name, effItem("cpu")) {
		t.Error("a resumed feedback Workload lost the gpu lane Verify gave it")
	}
}

// No changed file under a GPU path: the CPU gate only. Prefixes follow the
// planner's rule: one ending in "/" matches only under that directory.
func TestVerify_CPULaneNotTouchingGPUPathRunsCPUOnly(t *testing.T) {
	c := newEffectsClient(t, nil)
	g := &fakeGateRunner{}
	f := newFakeRunForge()
	o := fastOpts()
	o.GPUPaths = []string{"crates/gpu/"}
	e := NewClusterEffects(c, effNS, f, testTemplate(), g, o)
	name := dispatched(t, e, "cpu")
	branch := taskBranch(name, effIssue)
	f.changed[branch] = []string{"crates/gpu-docs/README.md", "src/lib.rs"}

	clean, _, err := e.Verify(context.Background(), name, branch)
	if err != nil || !clean {
		t.Fatalf("Verify = %v, %v; want clean", clean, err)
	}
	if len(g.calls) != 1 {
		t.Fatalf("gate runs = %+v, want the CPU gate only", g.calls)
	}
}

// Listing the changed files fails closed: an error is a Verify error, and no
// gate runs on a branch whose lane is unknown. With no GPU paths set, or for
// a gpu-lane item, nothing is listed.
func TestVerify_ChangedFilesErrorIsAnError(t *testing.T) {
	c := newEffectsClient(t, nil)
	g := &fakeGateRunner{}
	f := newFakeRunForge()
	f.changedErr = errors.New("compare unavailable")
	o := fastOpts()
	o.GPUPaths = []string{"crates/gpu/"}
	e := NewClusterEffects(c, effNS, f, testTemplate(), g, o)
	name := dispatched(t, e, "cpu")
	if clean, _, err := e.Verify(context.Background(), name, taskBranch(name, effIssue)); err == nil || clean {
		t.Fatalf("Verify = %v, %v; want an error", clean, err)
	}
	if len(g.calls) != 0 {
		t.Errorf("gate ran %d times after a failed listing", len(g.calls))
	}

	plain := NewClusterEffects(newEffectsClient(t, nil), effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	name = dispatched(t, plain, "cpu")
	if clean, _, err := plain.Verify(context.Background(), name, taskBranch(name, effIssue)); err != nil || !clean {
		t.Fatalf("no GPU paths: Verify = %v, %v; want clean without listing", clean, err)
	}
	gpu := NewClusterEffects(newEffectsClient(t, nil), effNS, f, testTemplate(), &fakeGateRunner{}, o)
	name = dispatched(t, gpu, "gpu")
	if clean, _, err := gpu.Verify(context.Background(), name, taskBranch(name, effIssue)); err != nil || !clean {
		t.Fatalf("gpu lane: Verify = %v, %v; want clean without listing", clean, err)
	}
}

// A failed Workload whose newest task is a review that requested changes is
// the review not converging; any other failure is not.
func TestWatch_ReviewRejectedOnlyWhenTheLastTaskIsANoGoReview(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	task := func(step string, kind meithealv1alpha1.AgenticTaskKind, at time.Time,
		verdict meithealv1alpha1.AgenticTaskVerdict) *meithealv1alpha1.AgenticTask {
		tk := taskWithResult(name, step, kind, `{"verdict":"`+string(verdict)+`"}`)
		tk.CreationTimestamp = metav1.NewTime(at)
		tk.Status.Verdict = verdict
		return tk
	}
	coder := func(at time.Time, v meithealv1alpha1.AgenticTaskVerdict) *meithealv1alpha1.AgenticTask {
		return task("code-42", meithealv1alpha1.AgenticTaskKindIssueFix, at, v)
	}
	review := func(at time.Time, v meithealv1alpha1.AgenticTaskVerdict) *meithealv1alpha1.AgenticTask {
		return task("review-42-0", meithealv1alpha1.AgenticTaskKindReview, at, v)
	}
	for label, tc := range map[string]struct {
		tasks []client.Object
		want  bool
	}{
		"no-go review last": {[]client.Object{
			coder(now.Add(-time.Hour), meithealv1alpha1.AgenticTaskVerdictGo),
			review(now.Add(-time.Minute), meithealv1alpha1.AgenticTaskVerdictNoGo)}, true},
		"coder failed, no review": {[]client.Object{
			coder(now.Add(-time.Hour), meithealv1alpha1.AgenticTaskVerdictIncomplete)}, false},
		"revision after the review failed": {[]client.Object{
			review(now.Add(-time.Hour), meithealv1alpha1.AgenticTaskVerdictNoGo),
			coder(now.Add(-time.Minute), meithealv1alpha1.AgenticTaskVerdictIncomplete)}, false},
		"approving review last": {[]client.Object{
			review(now.Add(-time.Minute), meithealv1alpha1.AgenticTaskVerdictGo)}, false},
		// The round's code and verify steps are emitted with its review.
		"no-go review with its round's steps": {[]client.Object{
			coder(now.Add(-time.Hour), meithealv1alpha1.AgenticTaskVerdictGo),
			task("verify-42", meithealv1alpha1.AgenticTaskKindVerify, now.Add(-time.Hour),
				meithealv1alpha1.AgenticTaskVerdictGatePass),
			review(now.Add(-time.Hour), meithealv1alpha1.AgenticTaskVerdictNoGo)}, true},
	} {
		objs := append([]client.Object{
			watchWorkload(name, now.Add(-2*time.Hour), meithealv1alpha1.WorkloadPhaseFailed)}, tc.tasks...)
		c := newEffectsClient(t, nil, objs...)
		o := fastOpts()
		o.Now = func() time.Time { return now }
		e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)
		res, err := e.Watch(context.Background(), name, time.Hour, 2.5)
		if err != nil {
			t.Fatalf("%s: Watch: %v", label, err)
		}
		if !res.WorkloadFailed || res.ReviewRejected != tc.want {
			t.Errorf("%s: WorkloadFailed=%v ReviewRejected=%v, want true and %v",
				label, res.WorkloadFailed, res.ReviewRejected, tc.want)
		}
	}
}
