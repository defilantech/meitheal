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
	"maps"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

const failWorkload = "wl-59"

// failTask is one child of a failed Workload as the watcher and the
// cascade-fail path leave it: a verdict with a result whose summary is set,
// or (summary "") a task with no result whose Completed condition carries
// msg.
func failTask(step string, v meithealv1alpha1.AgenticTaskVerdict, summary, msg string) meithealv1alpha1.AgenticTask {
	kind := meithealv1alpha1.AgenticTaskKindIssueFix
	switch {
	case strings.HasPrefix(step, "verify-"):
		kind = meithealv1alpha1.AgenticTaskKindVerify
	case strings.HasPrefix(step, "review-"):
		kind = meithealv1alpha1.AgenticTaskKindReview
	}
	t := meithealv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: failWorkload + "-" + step, Namespace: effNS,
			Labels: map[string]string{"meitheal.dev/workload": failWorkload, "meitheal.dev/step": step},
		},
		Spec:   meithealv1alpha1.AgenticTaskSpec{Kind: kind},
		Status: meithealv1alpha1.AgenticTaskStatus{Phase: meithealv1alpha1.AgenticTaskPhaseSucceeded, Verdict: v},
	}
	if summary != "" {
		raw, _ := json.Marshal(map[string]string{"verdict": string(v), "summary": summary})
		t.Status.Result = &runtime.RawExtension{Raw: raw}
	} else {
		t.Status.Phase = meithealv1alpha1.AgenticTaskPhaseFailed
	}
	if msg != "" {
		t.Status.Conditions = []metav1.Condition{{
			Type: "Completed", Status: metav1.ConditionFalse, Reason: "UpstreamFailed", Message: msg,
		}}
	}
	return t
}

// vGo, vNoGo, vIncomplete and vGatePass are declared in dispatch_test.go.
const (
	vGateFail  = meithealv1alpha1.AgenticTaskVerdictGateFail
	vGateError = meithealv1alpha1.AgenticTaskVerdictGateError
)

// twoNoGoRounds is the run from the bug report up to its last fix iteration:
// round 1 NO-GO, a fix that passed the gate, round 2 NO-GO.
func twoNoGoRounds() []meithealv1alpha1.AgenticTask {
	return []meithealv1alpha1.AgenticTask{
		failTask("code-59", vGo, "first attempt", ""),
		failTask("verify-59", vGatePass, "gate passed", ""),
		failTask("review-59-0", vNoGo, "round 1: missing a test", ""),
		failTask("code-59-r1", vGo, "added the test", ""),
		failTask("verify-59-r1", vGatePass, "gate passed", ""),
		failTask("review-59-0-r1", vNoGo, "round 2: the test asserts nothing", ""),
	}
}

func TestExplainFailure(t *testing.T) {
	long := strings.Repeat("x", summaryEvidenceCap+500)
	cases := []struct {
		name         string
		tasks        []meithealv1alpha1.AgenticTask
		maxIter      int
		wantReason   string
		wantEvidence map[string]string
	}{
		{
			name: "the bug report: round 2 NO-GO and the coder could not address it",
			tasks: append(twoNoGoRounds(),
				failTask("code-59-r2", vNoGo, "Cannot address the review: the spec forbids it", ""),
				failTask("verify-59-r2", vIncomplete, "", "dependency code-59-r2 failed"),
				failTask("review-59-0-r2", vIncomplete, "", "dependency verify-59-r2 failed"),
			),
			maxIter:    2,
			wantReason: "review returned NO-GO in round 2 and the coder could not address it",
			wantEvidence: map[string]string{
				"review": "round 2: the test asserts nothing",
				"coder":  "Cannot address the review: the spec forbids it",
			},
		},
		{
			name: "a fix iteration that crashed is not the coder declining",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGatePass, "gate passed", ""),
				failTask("review-59-0", vNoGo, "round 1: missing a test", ""),
				failTask("code-59-r1", vIncomplete, "", "executor error: out of turns"),
				failTask("verify-59-r1", vIncomplete, "", "dependency code-59-r1 failed"),
				failTask("review-59-0-r1", vIncomplete, "", "dependency verify-59-r1 failed"),
			},
			maxIter:    1,
			wantReason: "review returned NO-GO in round 1 and the coder's fix attempt failed",
			wantEvidence: map[string]string{
				"review": "round 1: missing a test",
				"coder":  "executor error: out of turns",
			},
		},
		{
			name:       "the iteration cap",
			tasks:      twoNoGoRounds(),
			maxIter:    1,
			wantReason: "review returned NO-GO after 2 rounds (iteration cap)",
			wantEvidence: map[string]string{
				"review": "round 2: the test asserts nothing",
				"coder":  "added the test",
			},
		},
		{
			name: "the iteration cap at zero iterations is one round",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGatePass, "gate passed", ""),
				failTask("review-59-0", vNoGo, "round 1: missing a test", ""),
			},
			maxIter:    0,
			wantReason: "review returned NO-GO after 1 round (iteration cap)",
			wantEvidence: map[string]string{
				"review": "round 1: missing a test",
				"coder":  "first attempt",
			},
		},
		{
			name: "a NO-GO below the cap that opened no iteration does not claim the cap",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGatePass, "gate passed", ""),
				failTask("review-59-0", vNoGo, "round 1: issue ask unverified", ""),
			},
			maxIter:    1,
			wantReason: "review returned NO-GO in round 1 and no fix iteration followed",
			wantEvidence: map[string]string{
				"review": "round 1: issue ask unverified",
				"coder":  "first attempt",
			},
		},
		{
			name: "the NO-GO reviewer's summary, not a GO reviewer's",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGatePass, "gate passed", ""),
				failTask("review-59-0", vGo, "looks fine", ""),
				failTask("review-59-1", vNoGo, "round 1: unsafe cast", ""),
			},
			maxIter:    0,
			wantReason: "review returned NO-GO after 1 round (iteration cap)",
			wantEvidence: map[string]string{
				"review": "round 1: unsafe cast",
				"coder":  "first attempt",
			},
		},
		{
			name: "the gate failed on the first attempt",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGateFail, "go vet failed", ""),
				failTask("review-59-0", vIncomplete, "", "dependency verify-59 failed"),
			},
			maxIter:    1,
			wantReason: "the gate returned GATE-FAIL on the coder's first attempt",
			wantEvidence: map[string]string{
				"gate":  "go vet failed",
				"coder": "first attempt",
			},
		},
		{
			name: "the gate errored on a fix iteration",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGatePass, "gate passed", ""),
				failTask("review-59-0", vNoGo, "round 1: missing a test", ""),
				failTask("code-59-r1", vGo, "added the test", ""),
				failTask("verify-59-r1", vGateError, "clone failed", ""),
				failTask("review-59-0-r1", vIncomplete, "", "dependency verify-59-r1 failed"),
			},
			maxIter:    1,
			wantReason: "the gate returned GATE-ERROR on the coder's fix for review round 1",
			wantEvidence: map[string]string{
				"gate":   "clone failed",
				"review": "round 1: missing a test",
				"coder":  "added the test",
			},
		},
		{
			name: "a coder error before any review keeps the old wording",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vIncomplete, "", "executor error: model unreachable"),
				failTask("verify-59", vIncomplete, "", "dependency code-59 failed"),
				failTask("review-59-0", vIncomplete, "", "dependency verify-59 failed"),
			},
			maxIter:      1,
			wantReason:   RunFailedReason,
			wantEvidence: map[string]string{"coder": "executor error: model unreachable"},
		},
		{
			name: "a coder NO-GO before any review keeps the old wording",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vNoGo, "already fixed upstream", ""),
				failTask("verify-59", vIncomplete, "", "dependency code-59 failed"),
			},
			maxIter:      1,
			wantReason:   RunFailedReason,
			wantEvidence: map[string]string{"coder": "already fixed upstream"},
		},
		{
			name: "a reviewer that crashed after an earlier NO-GO",
			tasks: append(twoNoGoRounds()[:3],
				failTask("code-59-r1", vGo, "added the test", ""),
				failTask("verify-59-r1", vGatePass, "gate passed", ""),
				failTask("review-59-0-r1", vIncomplete, "", "executor error: reviewer timed out"),
			),
			maxIter:    1,
			wantReason: "review returned NO-GO in round 1 and the next review did not return a verdict",
			wantEvidence: map[string]string{
				"review": "round 1: missing a test",
				"coder":  "added the test",
			},
		},
		{
			name: "an escalation attempt supersedes the base attempt",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vNoGo, "base coder gave up", ""),
				failTask("verify-59", vIncomplete, "", "dependency code-59 failed"),
				failTask("code-59-esc", vGo, "escalated fix", ""),
				failTask("verify-59-esc", vGatePass, "gate passed", ""),
				failTask("review-59-esc-0", vNoGo, "escalated review: wrong file", ""),
			},
			maxIter:    1,
			wantReason: "review returned NO-GO on the escalated coder's attempt",
			wantEvidence: map[string]string{
				"review": "escalated review: wrong file",
				"coder":  "escalated fix",
			},
		},
		{
			name: "the gate failed on the escalation attempt",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vNoGo, "base coder gave up", ""),
				failTask("code-59-esc", vGo, "escalated fix", ""),
				failTask("verify-59-esc", vGateFail, "tests failed", ""),
			},
			maxIter:    1,
			wantReason: "the gate returned GATE-FAIL on the escalated coder's attempt",
			wantEvidence: map[string]string{
				"gate":  "tests failed",
				"coder": "escalated fix",
			},
		},
		{
			name: "a task that is not an issue step is ignored",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, "first attempt", ""),
				failTask("verify-59", vGateFail, "go vet failed", ""),
				failTask("code-591x", vNoGo, "not a step", ""),
				failTask("review-59-x", vNoGo, "not a step", ""),
			},
			maxIter:    1,
			wantReason: "the gate returned GATE-FAIL on the coder's first attempt",
			wantEvidence: map[string]string{
				"gate":  "go vet failed",
				"coder": "first attempt",
			},
		},
		{
			name:         "no tasks at all",
			tasks:        nil,
			maxIter:      1,
			wantReason:   RunFailedReason,
			wantEvidence: nil,
		},
		{
			name: "summaries are capped",
			tasks: []meithealv1alpha1.AgenticTask{
				failTask("code-59", vGo, long, ""),
				failTask("verify-59", vGatePass, "gate passed", ""),
				failTask("review-59-0", vNoGo, long, ""),
			},
			maxIter:    0,
			wantReason: "review returned NO-GO after 1 round (iteration cap)",
			wantEvidence: map[string]string{
				"review": capString(long, summaryEvidenceCap),
				"coder":  capString(long, summaryEvidenceCap),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ev := explainFailure(tc.tasks, tc.maxIter)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if !maps.Equal(ev, tc.wantEvidence) {
				t.Errorf("evidence = %q, want %q", ev, tc.wantEvidence)
			}
		})
	}
}

// The order the tasks are listed in does not decide the outcome: rounds are
// read off the step names, so a reversed list explains the same failure.
func TestExplainFailure_IgnoresListOrder(t *testing.T) {
	tasks := append(twoNoGoRounds(),
		failTask("code-59-r2", vNoGo, "Cannot address the review", ""),
		failTask("verify-59-r2", vIncomplete, "", "dependency code-59-r2 failed"),
	)
	for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
		tasks[i], tasks[j] = tasks[j], tasks[i]
	}
	if got, _ := explainFailure(tasks, 2); got != "review returned NO-GO in round 2 and the coder could not address it" {
		t.Fatalf("reason = %q", got)
	}
}

// The reasons are public (the park comment carries them), so no task's own
// text ever reaches one.
func TestExplainFailure_ReasonCarriesNoTaskText(t *testing.T) {
	secret := "token=s3cr3t host=build-7.internal"
	tasks := []meithealv1alpha1.AgenticTask{
		failTask("code-59", vGo, secret, ""),
		failTask("verify-59", vGateFail, secret, ""),
		failTask("review-59-0", vNoGo, secret, ""),
		failTask("code-59-r1", vNoGo, secret, ""),
	}
	for n := range tasks {
		reason, _ := explainFailure(tasks[:n+1], 1)
		if strings.Contains(reason, "s3cr3t") || strings.Contains(reason, "wl-59") {
			t.Errorf("reason %q carries task text", reason)
		}
	}
}

// Watch reads the Workload's own iteration bound and hands the explanation to
// the driver with the failure.
func TestWatch_ExplainsAFailedWorkload(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	step := func(s string, v meithealv1alpha1.AgenticTaskVerdict, summary string) client.Object {
		tk := taskWithResult(name, s, meithealv1alpha1.AgenticTaskKindIssueFix,
			`{"verdict":"`+string(v)+`","summary":"`+summary+`"}`)
		tk.Status.Verdict = v
		switch {
		case strings.HasPrefix(s, "verify-"):
			tk.Spec.Kind = meithealv1alpha1.AgenticTaskKindVerify
		case strings.HasPrefix(s, "review-"):
			tk.Spec.Kind = meithealv1alpha1.AgenticTaskKindReview
		}
		return tk
	}
	w := watchWorkload(name, now.Add(-2*time.Hour), meithealv1alpha1.WorkloadPhaseFailed)
	w.Spec.MaxReviewIterations = ptr.To[int32](2)
	objs := []client.Object{w,
		step("code-42", vGo, "first attempt"),
		step("verify-42", vGatePass, "ok"),
		step("review-42-0", vNoGo, "round 1 findings"),
		step("code-42-r1", vGo, "fixed"),
		step("verify-42-r1", vGatePass, "ok"),
		step("review-42-0-r1", vNoGo, "round 2 findings"),
		step("code-42-r2", vNoGo, "Cannot address the review"),
	}
	c := newEffectsClient(t, nil, objs...)
	o := fastOpts()
	o.Now = func() time.Time { return now }
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)
	res, err := e.Watch(context.Background(), name, time.Hour, 2.5)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !res.WorkloadFailed {
		t.Fatal("WorkloadFailed = false")
	}
	if want := "review returned NO-GO in round 2 and the coder could not address it"; res.FailureReason != want {
		t.Errorf("FailureReason = %q, want %q", res.FailureReason, want)
	}
	want := map[string]string{"review": "round 2 findings", "coder": "Cannot address the review"}
	if !maps.Equal(res.FailureEvidence, want) {
		t.Errorf("FailureEvidence = %q, want %q", res.FailureEvidence, want)
	}

	// Unset, the bound is the controller's default of one iteration: the
	// same two NO-GO rounds without the third coder are the cap.
	w.Spec.MaxReviewIterations = nil
	c = newEffectsClient(t, nil, objs[:len(objs)-1]...)
	e = NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)
	if res, err = e.Watch(context.Background(), name, time.Hour, 2.5); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if want := "review returned NO-GO after 2 rounds (iteration cap)"; res.FailureReason != want {
		t.Errorf("default bound: FailureReason = %q, want %q", res.FailureReason, want)
	}
}
