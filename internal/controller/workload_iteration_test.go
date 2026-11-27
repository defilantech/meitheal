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

package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent"
	"github.com/defilantech/meitheal/pkg/agent/reviewer"
)

// iterationWorkload builds the issue-batch spec reviewIterationSteps
// consumes: coder + verifier refs plus `reviewers` base reviewer refs.
func iterationWorkload(issues []int32, reviewers int, maxIter *int32) *meithealv1alpha1.Workload {
	spec := meithealv1alpha1.WorkloadSpec{
		Intent:              "iteration unit test",
		Repo:                "defilantech/LLMKube",
		Issues:              issues,
		CoderAgentRef:       &corev1.LocalObjectReference{Name: "coder"},
		VerifierAgentRef:    &corev1.LocalObjectReference{Name: "gate"},
		MaxReviewIterations: maxIter,
	}
	for i := 0; i < reviewers; i++ {
		spec.ReviewerAgentRefs = append(spec.ReviewerAgentRefs, corev1.LocalObjectReference{Name: "reviewer"})
	}
	w := &meithealv1alpha1.Workload{Spec: spec}
	w.Name = "wl"
	return w
}

// reviewStep is the single reviewer's step label used throughout these
// tests. Every review-child builder below stamps it, so it lives here
// rather than being threaded through every call site.
const reviewStep = "review-641-0"

// demotionReasonIssueAsk / demotionReasonScopeDrift /
// demotionReasonUnverifiedSummary are the demotion rails' demotionReason
// strings, copied from enforceReviewerIssueAsk and
// enforceReviewerUnverifiedSummary (pkg/agent/executor_native.go)
// and enforceReviewerScopeOverlap (pkg/agent/scope_overlap.go).
// Only the prose is copied; the verdictDemotedBy values that actually drive
// the predicate come from the shared reviewer constants both sides compile
// against.
const (
	demotionReasonIssueAsk = "issueAsk could not be verified as covering the " +
		"fetched issue body; review verdict is untrusted"
	demotionReasonScopeDrift = "scope drift: the issue names 1 file(s) " +
		"(internal/controller/workload_iteration.go) and the diff touches none of them"
	demotionReasonUnverifiedSummary = "reviewer summary states \"cannot verify goal reward or " +
		"progression logic\"; a GO whose own summary reports verification could " +
		"not be performed is not a verified approval"
)

// reviewResultRaw renders the status.result a reviewer task really carries,
// by marshalling an actual agent.Result rather than hand-writing JSON. The
// Extra map mirrors NativeAgentLoopExecutor.modelDecidedResult
// (pkg/agent/executor_native.go): the rails stamp their keys into
// LoopResult.Terminal.Extra, and modelDecidedResult nests that WHOLE map one
// level down under "modelExtra", promoting only outcome / unverified /
// resolvedBy to the top level. modelDecidedResult is an unexported method,
// so the controller test cannot call it; the nesting is pinned against the
// real executor by TestIssueAskDemotionLandsUnderModelExtra in
// pkg/agent, which runs the actual rail and the actual
// modelDecidedResult and asserts the JSON path this predicate reads.
//
// #1641: the first cut of these fixtures hand-wrote verdictDemoted as a
// SIBLING of modelExtra, a shape the executor never emits, so the suppression
// predicate passed its tests and did nothing in production.
func reviewResultRaw(
	verdict meithealv1alpha1.AgenticTaskVerdict, summary string, modelExtra map[string]any,
) *runtime.RawExtension {
	res := agent.NewResult("review", verdict, summary, 42*time.Second)
	res.Extra = map[string]any{
		"outcome":       "MODEL-DECIDED",
		"transcriptRef": map[string]any{"kind": "ConfigMap", "name": "wl-" + reviewStep + "-transcript"},
		"turnCount":     7,
		"modelExtra":    modelExtra,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		panic("marshal review result fixture: " + err.Error())
	}
	return &runtime.RawExtension{Raw: raw}
}

// railDemotedNoGoChild is a terminal NO-GO review child whose NO-GO a
// harness rail MANUFACTURED rather than the reviewer asserting it: the rail
// stamped verdictDemoted plus verdictDemotedBy naming itself, and
// verdictClaimed archiving the verdict it rewrote (#1636).
//
// rail, claimed, and findingsJSON are parameters because the predicate turns
// on all three: any of the demoting rails (issueAsk, scope-overlap,
// unverified-summary) rewriting a GO is inert, and only while the demotion
// carries nothing the coder could act on.
func railDemotedNoGoChild(
	rail, claimed, reason, summary, findingsJSON string,
) meithealv1alpha1.AgenticTask {
	c := child(reviewStep, meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictNoGo)
	var findings any
	if err := json.Unmarshal([]byte(findingsJSON), &findings); err != nil {
		panic("bad findings fixture JSON: " + err.Error())
	}
	c.Status.Result = reviewResultRaw(meithealv1alpha1.AgenticTaskVerdictNoGo, summary, map[string]any{
		"issueAskVerified": false,
		"verdictDemoted":   true,
		"verdictDemotedBy": rail,
		"verdictClaimed":   claimed,
		"demotionReason":   reason,
		"findings":         findings,
	})
	return c
}

// demotedNoGoChild is the inert case: the issueAsk rail rewrote the
// reviewer's GO.
func demotedNoGoChild(summary, findingsJSON string) meithealv1alpha1.AgenticTask {
	return railDemotedNoGoChild(reviewer.RailIssueAsk, string(meithealv1alpha1.AgenticTaskVerdictGo),
		demotionReasonIssueAsk, summary, findingsJSON)
}

// untrustedNoGoChild is the reviewer's OWN NO-GO that a rail kept but marked
// untrusted (#41): rail names the rail, claimed is the archived verdict, and
// more carries any extra modelExtra keys (groundedFindingDemotion).
func untrustedNoGoChild(
	step, rail, claimed, reason, summary, findingsJSON string, more map[string]any,
) meithealv1alpha1.AgenticTask {
	c := railDemotedNoGoChild(rail, claimed, reason, summary, findingsJSON)
	var envelope struct {
		Extra map[string]any `json:"extra"`
	}
	if err := json.Unmarshal(c.Status.Result.Raw, &envelope); err != nil {
		panic("decode fixture: " + err.Error())
	}
	me, _ := envelope.Extra["modelExtra"].(map[string]any)
	for k, v := range more {
		me[k] = v
	}
	c.Status.Result = reviewResultRaw(meithealv1alpha1.AgenticTaskVerdictNoGo, summary, me)
	return withStep(c, step)
}

// issueAskUntrusted is the enforceReviewerIssueAsk non-GO path: the reviewer
// said NO-GO itself, and the rail could not verify it read the issue.
func issueAskUntrusted(step, summary string) meithealv1alpha1.AgenticTask {
	return untrustedNoGoChild(step, reviewer.RailIssueAsk, string(meithealv1alpha1.AgenticTaskVerdictNoGo),
		demotionReasonIssueAsk, summary, ghostFindings, nil)
}

// approvingReviewChild is a terminal GO review carrying a clean result.
func approvingReviewChild(step string) meithealv1alpha1.AgenticTask {
	c := child(step, meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictGo)
	c.Status.Result = reviewResultRaw(meithealv1alpha1.AgenticTaskVerdictGo, "APPROVE: the change matches the issue",
		map[string]any{"findings": []any{}, "issueAskVerified": true})
	return c
}

// ghostFindings is the #41 review: blockers about files the branch never
// touched.
const ghostFindings = `[{"severity":"blocker","area":"scope","message":"the reconciler drops the retry budget",` +
	`"file":"internal/controller/ghost.go","line":42}]`

// withStep re-labels a review child for a later fix iteration, so a case can
// place one of the builders above in round r1 instead of the base round.
func withStep(c meithealv1alpha1.AgenticTask, step string) meithealv1alpha1.AgenticTask {
	c.Name = "wl-" + step
	c.Labels[labelStep] = step
	return c
}

// noGoChild is a terminal NO-GO review child carrying a structured
// review result so the feedback-prompt path is exercised end to end.
func noGoChild(summary, findingsJSON string) meithealv1alpha1.AgenticTask {
	c := child(reviewStep, meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictNoGo)
	var findings any
	if err := json.Unmarshal([]byte(findingsJSON), &findings); err != nil {
		panic("bad findings fixture JSON: " + err.Error())
	}
	c.Status.Result = reviewResultRaw(meithealv1alpha1.AgenticTaskVerdictNoGo, summary,
		map[string]any{"findings": findings})
	return c
}

func TestReviewIterationSteps(t *testing.T) {
	succeeded := meithealv1alpha1.AgenticTaskPhaseSucceeded
	running := meithealv1alpha1.AgenticTaskPhaseRunning
	noGo := meithealv1alpha1.AgenticTaskVerdictNoGo
	gateFail := meithealv1alpha1.AgenticTaskVerdictGateFail
	incomplete := meithealv1alpha1.AgenticTaskVerdictIncomplete
	gatePass := meithealv1alpha1.AgenticTaskVerdictGatePass
	goVerdict := meithealv1alpha1.AgenticTaskVerdictGo

	baseRound := func(reviewVerdict meithealv1alpha1.AgenticTaskVerdict) []meithealv1alpha1.AgenticTask {
		return []meithealv1alpha1.AgenticTask{
			child("code-641", succeeded, goVerdict),
			child("verify-641", succeeded, gatePass),
			child("review-641-0", succeeded, reviewVerdict),
		}
	}

	cases := []struct {
		name         string
		w            *meithealv1alpha1.Workload
		children     []meithealv1alpha1.AgenticTask
		wantSteps    []string // expected step names, in order
		wantIterated []int32
		// wantSuppressed lists the step labels reviewIterationSteps must
		// report as inert demotions, in order. nil asserts none.
		wantSuppressed []string
		// wantRereviewed / wantUntrusted are the issues re-reviewed and
		// the re-reviews that came back untrusted again (#41).
		wantRereviewed []int32
		wantUntrusted  []string
	}{
		{
			name:         "reviewer NO-GO triggers the full r1 triple",
			w:            iterationWorkload([]int32{641}, 1, nil),
			children:     baseRound(noGo),
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name:     "reviewer GO converges: no iteration",
			w:        iterationWorkload([]int32{641}, 1, nil),
			children: baseRound(goVerdict),
		},
		{
			// #1636: the issueAsk rail rewrites an approved verdict to
			// NO-GO. With no findings the coder is handed a rejection it
			// cannot act on, and re-running cannot make an unverifiable
			// issueAsk verify, so the loop has no terminating condition.
			name: "issueAsk demotion of a GO carrying no findings does not iterate",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				demotedNoGoChild("APPROVE: changes are minimal and well-tested", `[]`),
			},
			wantSuppressed: []string{reviewStep},
		},
		{
			// Narrowness guard: a demotion is only inert when it carries
			// nothing to fix. Real findings still deserve an iteration.
			name: "issueAsk demotion carrying findings still iterates",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				demotedNoGoChild("mixed",
					`[{"severity":"major","area":"scope","message":"nil deref on empty strategy"}]`),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			// #1684: a scope-overlap demotion of a GO carrying NO findings
			// is inert, exactly like the issueAsk case (#1636). An issue
			// that names the file where a bug is OBSERVED while the fix
			// belongs where the bug is CAUSED demotes the correct diff; the
			// coder cannot act on the bare drift, so suppressing the
			// iteration (instead of opening an unconvergeable revision
			// cycle) is the right consequence. Detection still happens; only
			// the consequence weakens.
			name: "scope-overlap demotion of a GO with no findings is inert",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				railDemotedNoGoChild(reviewer.RailScopeOverlap, string(goVerdict),
					demotionReasonScopeDrift, "APPROVE: looks good to me", `[]`),
			},
			wantSuppressed: []string{reviewStep},
		},
		{
			// #1684 safety boundary: a scope-overlap demotion is only inert
			// when it carries nothing to fix. A demotion WITH findings names
			// concrete work the coder can address, so it must still open an
			// iteration even though the rail is scope-overlap.
			name: "scope-overlap demotion carrying findings still iterates",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				railDemotedNoGoChild(reviewer.RailScopeOverlap, string(goVerdict),
					demotionReasonScopeDrift, "scope drift plus a real defect",
					`[{"severity":"major","area":"scope","message":"the fix must also touch the config loader"}]`),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			// #1454: the unverified-summary rail rewrites a GO to NO-GO when
			// the reviewer's own terminal summary admits verification could
			// not be performed. That is a statement about the review
			// environment, not the change: with no findings the coder is
			// handed a rejection it cannot act on, and re-running cannot
			// make an unverifiable environment verifiable, so the iteration
			// is suppressed (#1636). Before this rail's name reached the
			// inertDemotion predicate, this exact envelope drove a futile
			// fix iteration.
			name: "unverified-summary demotion of a GO with no findings is inert",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				railDemotedNoGoChild(reviewer.RailUnverifiedSummary, string(goVerdict),
					demotionReasonUnverifiedSummary,
					"cannot verify goal reward or progression logic", `[]`),
			},
			wantSuppressed: []string{reviewStep},
		},
		{
			// Narrowness guard, mirroring the issueAsk and scope-overlap
			// pairs: a demotion that carries findings names concrete work
			// the coder can address, so it must still open an iteration
			// even though the rail is unverified-summary.
			name: "unverified-summary demotion carrying findings still iterates",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				railDemotedNoGoChild(reviewer.RailUnverifiedSummary, string(goVerdict),
					demotionReasonUnverifiedSummary,
					"verification could not be performed, but the diff has a real defect",
					`[{"severity":"major","area":"correctness","message":"the retry loop drops the last batch"}]`),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			// Claimed-verdict guard (#1641): enforceReviewerIssueAsk stamps
			// the demotion fields on the path where it does NOT rewrite the
			// verdict, marking an unverified NON-GO review untrusted and
			// returning the reviewer's own NO-GO (verdictClaimed=NO-GO).
			// It is not inert (the rejection may be genuine), but the
			// harness does not believe it either, so it is re-reviewed once
			// rather than handed to the coder (#41).
			name: "issueAsk marking an untrusted NO-GO it did not rewrite re-reviews instead of iterating",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				railDemotedNoGoChild(reviewer.RailIssueAsk, string(noGo),
					demotionReasonIssueAsk, "REJECT: the fix papers over the race instead of fixing it", `[]`),
			},
			wantSteps:      []string{"review-641-0-rr"},
			wantRereviewed: []int32{641},
		},
		{
			// #41 as observed: an unverified issueAsk on the reviewer's own
			// NO-GO whose blockers cite files the branch never touched.
			name: "untrusted NO-GO carrying findings re-reviews instead of iterating",
			w:    iterationWorkload([]int32{641}, 1, ptr.To(int32(2))),
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				issueAskUntrusted("review-641-0-r1", "REQUEST-CHANGES: three blockers"),
			),
			wantSteps:      []string{"review-641-0-r1-rr"},
			wantRereviewed: []int32{641},
		},
		{
			// The out-of-diff rail marks a NO-GO untrusted even when the
			// issueAsk verified.
			name: "out-of-diff NO-GO re-reviews instead of iterating",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				untrustedNoGoChild(reviewStep, reviewer.RailOutOfDiffFindings, string(noGo),
					"every blocking finding cites a file the branch diff does not touch "+
						"(internal/controller/ghost.go); review verdict is untrusted",
					"REJECT: wrong files", ghostFindings, nil),
			},
			wantSteps:      []string{"review-641-0-rr"},
			wantRereviewed: []int32{641},
		},
		{
			// The grounded-finding rail demoted the reviewer's NO-GO to GO
			// (no blocker cites a changed line), then the issueAsk rail
			// rewrote that GO back to NO-GO. The findings are the ones the
			// grounded rail rejected; pinned on the producer side by
			// TestUngroundedNoGoThenUnverifiedIssueAsk_Shape.
			name: "ungrounded NO-GO re-flagged by issueAsk re-reviews instead of iterating",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				untrustedNoGoChild(reviewStep, reviewer.RailIssueAsk, string(goVerdict),
					demotionReasonIssueAsk, "REQUEST-CHANGES: wrong files", ghostFindings,
					map[string]any{
						"groundedFindingDemotion": true,
						"groundedFindingReason":   "NO-GO demoted to GO: 0 of 1 blocking finding(s) cite a changed line",
					}),
			},
			wantSteps:      []string{"review-641-0-rr"},
			wantRereviewed: []int32{641},
		},
		{
			name: "re-review in flight: nothing more is emitted",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				issueAskUntrusted(reviewStep, "REJECT"),
				child("review-641-0-rr", running, ""),
			},
		},
		{
			name: "trusted re-review GO converges: no iteration",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				issueAskUntrusted(reviewStep, "REJECT"),
				approvingReviewChild("review-641-0-rr"),
			},
		},
		{
			name: "trusted re-review NO-GO iterates as any review does",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				issueAskUntrusted(reviewStep, "REJECT"),
				withStep(noGoChild("the retry path is untested",
					`[{"severity":"major","area":"tests","message":"no test covers the retry"}]`), "review-641-0-rr"),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "untrusted re-review ends the round without an iteration",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				issueAskUntrusted(reviewStep, "REJECT"),
				issueAskUntrusted("review-641-0-rr", "REJECT again"),
			},
			wantUntrusted: []string{"review-641-0-rr"},
		},
		{
			// The re-review asks whether to believe the rejection, not
			// whether the coder gets another attempt, so it runs in the last
			// round of the budget too.
			name: "untrusted NO-GO in the final round is still re-reviewed",
			w:    iterationWorkload([]int32{641}, 1, nil), // nil -> 1 iteration
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				issueAskUntrusted("review-641-0-r1", "REJECT"),
			),
			wantSteps:      []string{"review-641-0-r1-rr"},
			wantRereviewed: []int32{641},
		},
		{
			// A trusted NO-GO in the same round iterates on its own; the
			// iteration brings a fresh review round, so nothing is re-run.
			name: "trusted NO-GO beside an untrusted one iterates without a re-review",
			w:    iterationWorkload([]int32{641}, 2, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				withStep(noGoChild("real defect", `[]`), "review-641-0"),
				issueAskUntrusted("review-641-1", "REJECT"),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1", "review-641-1-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "only the untrusted reviewer is re-run",
			w:    iterationWorkload([]int32{641}, 2, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				child("review-641-0", succeeded, goVerdict),
				issueAskUntrusted("review-641-1", "REJECT"),
			},
			wantSteps:      []string{"review-641-1-rr"},
			wantRereviewed: []int32{641},
		},
		{
			name: "explicit 0 disables the re-review along with iteration",
			w:    iterationWorkload([]int32{641}, 1, ptr.To(int32(0))),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				issueAskUntrusted(reviewStep, "REJECT"),
			},
		},
		{
			// Narrowness guard: an UNdemoted NO-GO is the reviewer's own
			// judgement. Absent structured findings it still iterates, as
			// it did before #1636; the summary carries the feedback.
			name: "genuine NO-GO with no findings still iterates",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, gatePass),
				noGoChild("rejected: the fix does not address the issue", `[]`),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "waits for every reviewer in the round to be terminal",
			w:    iterationWorkload([]int32{641}, 2, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-641-1", running, ""),
			},
		},
		{
			name: "cascade INCOMPLETE after GATE-FAIL does not iterate",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("verify-641", succeeded, gateFail),
				child("review-641-0", succeeded, incomplete),
			},
		},
		{
			name: "existing r1 children block re-emission (idempotency)",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: append(baseRound(noGo),
				child("code-641-r1", running, ""),
				child("verify-641-r1", "", ""),
				child("review-641-0-r1", "", ""),
			),
		},
		{
			name: "partial create failure repaired: only the missing r1 steps re-emit",
			w:    iterationWorkload([]int32{641}, 1, nil),
			children: append(baseRound(noGo),
				child("code-641-r1", running, ""),
			),
			wantSteps:    []string{"verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "r1 NO-GO with budget left chains r2",
			w:    iterationWorkload([]int32{641}, 1, ptr.To(int32(2))),
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				child("review-641-0-r1", succeeded, noGo),
			),
			wantSteps:    []string{"code-641-r2", "verify-641-r2", "review-641-0-r2"},
			wantIterated: []int32{641},
		},
		{
			name: "r1 NO-GO with budget exhausted emits nothing (fails as today)",
			w:    iterationWorkload([]int32{641}, 1, nil), // nil -> 1 iteration
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				child("review-641-0-r1", succeeded, noGo),
			),
		},
		{
			// The LAST round in the budget is never revisited by a k+1 walk,
			// so its suppression has to be scanned where the walk ends or the
			// Workload fails with no explanation after all (#1636).
			name: "inert demotion in the final round is still reported",
			w:    iterationWorkload([]int32{641}, 1, nil), // nil -> 1 iteration
			children: append(baseRound(noGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, gatePass),
				withStep(demotedNoGoChild("APPROVE: the revision addresses every point", `[]`),
					"review-641-0-r1"),
			),
			wantSuppressed: []string{"review-641-0-r1"},
		},
		{
			name:     "explicit 0 disables iteration",
			w:        iterationWorkload([]int32{641}, 1, ptr.To(int32(0))),
			children: baseRound(noGo),
		},
		{
			name: "per-issue isolation: only the NO-GO issue iterates",
			w:    iterationWorkload([]int32{641, 642}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
				child("review-642-0", succeeded, goVerdict),
			},
			wantSteps:    []string{"code-641-r1", "verify-641-r1", "review-641-0-r1"},
			wantIterated: []int32{641},
		},
		{
			name: "issue number prefixes do not cross-match (64 vs 641)",
			w:    iterationWorkload([]int32{64}, 1, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("review-641-0", succeeded, noGo),
			},
		},
		{
			name: "no reviewers configured is inert",
			w:    iterationWorkload([]int32{641}, 0, nil),
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := reviewIterationSteps(tc.w, tc.children)
			steps, iterated, suppressed := plan.steps, plan.iterated, plan.suppressed
			if got, want := joinInt32(plan.rereviewed), joinInt32(tc.wantRereviewed); got != want {
				t.Fatalf("rereviewed = [%s], want [%s]", got, want)
			}
			var gotUntrusted []string
			for _, u := range plan.untrusted {
				gotUntrusted = append(gotUntrusted, u.step)
				if u.reason == "" {
					t.Errorf("untrusted %s carries no reason; the condition would name no cause", u.step)
				}
			}
			if strings.Join(gotUntrusted, ",") != strings.Join(tc.wantUntrusted, ",") {
				t.Fatalf("untrusted = %v, want %v", gotUntrusted, tc.wantUntrusted)
			}

			var gotSuppressed []string
			for _, s := range suppressed {
				gotSuppressed = append(gotSuppressed, s.step)
				// A suppression the controller cannot explain is the gap
				// #1636 opened: the demoted NO-GO still fails the Workload.
				if s.reason == "" {
					t.Errorf("suppressed %s carries no demotionReason; the condition would name no cause", s.step)
				}
			}
			if len(gotSuppressed) != len(tc.wantSuppressed) {
				t.Fatalf("suppressed = %v, want %v", gotSuppressed, tc.wantSuppressed)
			}
			for i := range tc.wantSuppressed {
				if gotSuppressed[i] != tc.wantSuppressed[i] {
					t.Fatalf("suppressed = %v, want %v", gotSuppressed, tc.wantSuppressed)
				}
			}

			var gotNames []string
			for _, s := range steps {
				gotNames = append(gotNames, s.Name)
			}
			if len(gotNames) != len(tc.wantSteps) {
				t.Fatalf("steps = %v, want %v", gotNames, tc.wantSteps)
			}
			for i := range tc.wantSteps {
				if gotNames[i] != tc.wantSteps[i] {
					t.Fatalf("steps = %v, want %v", gotNames, tc.wantSteps)
				}
			}
			if len(iterated) != len(tc.wantIterated) {
				t.Fatalf("iterated = %v, want %v", iterated, tc.wantIterated)
			}
			for i := range tc.wantIterated {
				if iterated[i] != tc.wantIterated[i] {
					t.Fatalf("iterated = %v, want %v", iterated, tc.wantIterated)
				}
			}

			// Every emitted coder step must re-target the same branch
			// with allowOverwrite + a non-empty feedback prompt; verify
			// and review steps chain behind it within the iteration.
			for _, s := range steps {
				if s.Payload.Branch == "" || !strings.HasPrefix(s.Payload.Branch, "meitheal/wl/issue-") {
					t.Errorf("step %s branch = %q, want the original issue branch", s.Name, s.Payload.Branch)
				}
				switch s.Kind {
				case meithealv1alpha1.AgenticTaskKindIssueFix:
					if !s.Payload.AllowOverwrite {
						t.Errorf("step %s must set allowOverwrite to amend its own branch", s.Name)
					}
					if s.Payload.ReviseFromBranch != s.Payload.Branch {
						t.Errorf("step %s reviseFromBranch = %q, want %q so the executor restores the prior attempt (#951)",
							s.Name, s.Payload.ReviseFromBranch, s.Payload.Branch)
					}
					if !strings.Contains(s.Payload.Prompt, "NO-GO") {
						t.Errorf("step %s prompt must carry the review feedback, got %q", s.Name, s.Payload.Prompt)
					}
					if len(s.DependsOn) != 0 {
						t.Errorf("step %s dependsOn = %v, want none (prior round is terminal)", s.Name, s.DependsOn)
					}
				case meithealv1alpha1.AgenticTaskKindVerify, meithealv1alpha1.AgenticTaskKindReview:
					if len(s.DependsOn) != 1 {
						t.Errorf("step %s dependsOn = %v, want exactly one upstream", s.Name, s.DependsOn)
					}
				}
			}
		})
	}
}

// TestReviewIterationCoderRef covers the revision profile pairing
// (#951): iteration coder steps reference spec.revisionCoderAgentRef
// when set (a revision amends restored work and wants a revision-tuned
// profile) and fall back to spec.coderAgentRef otherwise. Verify and
// review steps keep their own refs either way.
func TestReviewIterationCoderRef(t *testing.T) {
	noGoRound := []meithealv1alpha1.AgenticTask{
		child("code-641", meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictGo),
		child("verify-641", meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0", meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictNoGo),
	}

	cases := []struct {
		name          string
		revisionRef   *corev1.LocalObjectReference
		wantCoderName string
	}{
		{"revisionCoderAgentRef set selects the revision coder", &corev1.LocalObjectReference{Name: "revision-coder"}, "revision-coder"},
		{"unset falls back to coderAgentRef", nil, "coder"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := iterationWorkload([]int32{641}, 1, nil)
			w.Spec.RevisionCoderAgentRef = tc.revisionRef

			steps := reviewIterationSteps(w, noGoRound).steps
			refs := map[string]string{}
			for _, s := range steps {
				refs[s.Name] = s.AgentRef.Name
			}
			if got := refs["code-641-r1"]; got != tc.wantCoderName {
				t.Errorf("code-641-r1 agentRef = %q, want %q", got, tc.wantCoderName)
			}
			if got := refs["verify-641-r1"]; got != "gate" {
				t.Errorf("verify-641-r1 agentRef = %q, want %q", got, "gate")
			}
			if got := refs["review-641-0-r1"]; got != "reviewer" {
				t.Errorf("review-641-0-r1 agentRef = %q, want %q", got, "reviewer")
			}
		})
	}
}

func TestReviewFeedbackPrompt(t *testing.T) {
	structured := noGoChild("scope creep beyond the issue ask",
		`[{"severity":"blocker","area":"scope","message":"reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30, unrelated to the issue","file":"config/auth.py","line":12,"suggestion":"revert the unrelated change"}]`)
	prompt := reviewFeedbackPrompt([]*meithealv1alpha1.AgenticTask{&structured})
	for _, want := range []string{
		"rejected the previous attempt",
		"NO-GO",
		// The executor's revise-from-branch restore (#951) means the
		// workspace really does start from the prior attempt; the prompt
		// must direct a delta, not a rebuild.
		"restored from this task's branch",
		"Do not rebuild the fix from scratch",
		"Amend the existing work",
		"scope creep beyond the issue ask",
		"[blocker/scope] reduces ACCESS_TOKEN_EXPIRE_MINUTES from 10080 to 30",
		"config/auth.py:12",
		"revert the unrelated change",
		"Address this feedback",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// Legacy map-shaped findings (the boolean + *_details shape from the
	// issue report) fail the strict schema and must fall back to raw JSON
	// rather than vanishing.
	legacy := noGoChild("missing tests",
		`{"missing_tests":true,"missing_tests_details":"no unit test covers the new branch"}`)
	prompt = reviewFeedbackPrompt([]*meithealv1alpha1.AgenticTask{&legacy})
	if !strings.Contains(prompt, "no unit test covers the new branch") {
		t.Errorf("legacy findings must surface via the raw-JSON fallback:\n%s", prompt)
	}

	// A result-less NO-GO still yields a usable prompt.
	bare := child("review-641-1", meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskVerdictNoGo)
	prompt = reviewFeedbackPrompt([]*meithealv1alpha1.AgenticTask{&bare})
	if !strings.Contains(prompt, "Reviewer review-641-1") {
		t.Errorf("result-less reviewer must still be named:\n%s", prompt)
	}
}

func TestActiveChildren(t *testing.T) {
	succeeded := meithealv1alpha1.AgenticTaskPhaseSucceeded
	w := iterationWorkload([]int32{641}, 1, nil)

	children := []meithealv1alpha1.AgenticTask{
		child("code-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
		child("verify-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0", succeeded, meithealv1alpha1.AgenticTaskVerdictNoGo),
		child("code-641-r1", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
		child("verify-641-r1", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0-r1", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
		child("escalate-641-0", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
	}

	active := activeChildren(w, children)
	names := make([]string, 0, len(active))
	for i := range active {
		names = append(names, active[i].Labels[labelStep])
	}
	want := []string{"code-641-r1", "verify-641-r1", "review-641-0-r1", "escalate-641-0"}
	if len(names) != len(want) {
		t.Fatalf("active = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("active = %v, want %v", names, want)
		}
	}

	// Without a later iteration nothing is filtered.
	base := children[:3]
	if got := activeChildren(w, base); len(got) != 3 {
		t.Fatalf("no-iteration input must pass through, got %d children", len(got))
	}

	// Explicit Pipeline mode passes through untouched even when names
	// resemble the synthesized scheme.
	pw := iterationWorkload([]int32{641}, 1, nil)
	pw.Spec.Pipeline = []meithealv1alpha1.PipelineStep{{Name: "code-641"}}
	if got := activeChildren(pw, children); len(got) != len(children) {
		t.Fatalf("pipeline mode must not filter, got %d of %d", len(got), len(children))
	}
}

// TestActiveChildren_EscalationSupersedesBase proves the #963 rule: once
// a code-<n>-esc task exists, the failed base attempt (code/verify/review
// -<n>) is dropped from the active slice so its cascade-failure does not
// pin the rollup at Failed, while the -esc attempt and unescalated issues
// are untouched.
func TestActiveChildren_EscalationSupersedesBase(t *testing.T) {
	succeeded := meithealv1alpha1.AgenticTaskPhaseSucceeded
	failed := meithealv1alpha1.AgenticTaskPhaseFailed
	noGo := meithealv1alpha1.AgenticTaskVerdictNoGo
	gatePass := meithealv1alpha1.AgenticTaskVerdictGatePass
	goVerdict := meithealv1alpha1.AgenticTaskVerdictGo

	w := iterationWorkload([]int32{944, 921}, 1, nil)

	children := []meithealv1alpha1.AgenticTask{
		// Issue 944: base coder NO-GO, verify/review cascade-failed, then
		// escalated. The base triple must be superseded.
		child("code-944", succeeded, noGo),
		child("verify-944", failed, ""),
		child("review-944-0", failed, ""),
		child("code-944-esc", succeeded, goVerdict),
		child("verify-944-esc", succeeded, gatePass),
		child("review-944-esc-0", succeeded, goVerdict),
		// Issue 921: not escalated, must pass through untouched.
		child("code-921", succeeded, goVerdict),
		child("verify-921", succeeded, gatePass),
		child("review-921-0", succeeded, goVerdict),
	}

	active := activeChildren(w, children)
	got := make(map[string]bool, len(active))
	for i := range active {
		got[active[i].Labels[labelStep]] = true
	}

	dropped := []string{"code-944", "verify-944", "review-944-0"}
	for _, name := range dropped {
		if got[name] {
			t.Errorf("base step %q must be superseded by the escalation, but it is still active", name)
		}
	}
	kept := []string{
		"code-944-esc", "verify-944-esc", "review-944-esc-0",
		"code-921", "verify-921", "review-921-0",
	}
	for _, name := range kept {
		if !got[name] {
			t.Errorf("step %q must remain active, but it was filtered", name)
		}
	}
	if len(active) != len(kept) {
		t.Fatalf("active = %d steps, want %d (%v)", len(active), len(kept), kept)
	}
}

// TestParseReviewStep pins the re-review naming (#41) against the parsers
// every consumer of review step names goes through: a re-review belongs to
// the round it re-runs, and nothing else changes classification.
func TestParseReviewStep(t *testing.T) {
	cases := []struct {
		step         string
		wantOK       bool
		wantIdx      int
		wantK        int
		wantRereview bool
	}{
		{step: "review-641-0", wantOK: true},
		{step: "review-641-2-r3", wantOK: true, wantIdx: 2, wantK: 3},
		{step: "review-641-0-rr", wantOK: true, wantRereview: true},
		{step: "review-641-1-r2-rr", wantOK: true, wantIdx: 1, wantK: 2, wantRereview: true},
		{step: "review-641-0-rr-rr"},
		{step: "review-641-0-r0"},
		{step: "review-641-esc-0"},
		{step: "review-641-esc-0-rr"},
		{step: "review-6410-0-rr"},
		{step: "review-641--rr"},
		{step: "escalate-641-0"},
		{step: "code-641-rr"},
	}
	for _, tc := range cases {
		t.Run(tc.step, func(t *testing.T) {
			idx, k, rereview, ok := parseReviewStep(tc.step, 641)
			if ok != tc.wantOK || idx != tc.wantIdx || k != tc.wantK || rereview != tc.wantRereview {
				t.Fatalf("parseReviewStep(%q) = (%d, %d, %v, %v), want (%d, %d, %v, %v)", tc.step,
					idx, k, rereview, ok, tc.wantIdx, tc.wantK, tc.wantRereview, tc.wantOK)
			}
			gotK, gotOK := issueStepIteration(tc.step, 641)
			if gotOK != tc.wantOK || gotK != tc.wantK {
				t.Fatalf("issueStepIteration(%q) = (%d, %v), want (%d, %v)", tc.step, gotK, gotOK, tc.wantK, tc.wantOK)
			}
		})
	}
	if name := rereviewStepName(641, 1, 2); name != "review-641-1-r2-rr" {
		t.Fatalf("rereviewStepName = %q", name)
	}
}

// TestRereviewStepShape: the re-review is a fresh task for the same reviewer
// Agent on the same branch, behind the same gate as the review it re-runs,
// and it still opens the PR when it approves (#937).
func TestRereviewStepShape(t *testing.T) {
	succeeded := meithealv1alpha1.AgenticTaskPhaseSucceeded
	goVerdict := meithealv1alpha1.AgenticTaskVerdictGo
	cases := []struct {
		name     string
		gateless bool
		children []meithealv1alpha1.AgenticTask
		wantName string
		wantDep  string
	}{
		{
			name: "base round, gated",
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				child("verify-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
				issueAskUntrusted(reviewStep, "REJECT"),
			},
			wantName: "review-641-0-rr",
			wantDep:  "verify-641",
		},
		{
			name:     "base round, gateless",
			gateless: true,
			children: []meithealv1alpha1.AgenticTask{
				child("code-641", succeeded, goVerdict),
				issueAskUntrusted(reviewStep, "REJECT"),
			},
			wantName: "review-641-0-rr",
			wantDep:  "code-641",
		},
		{
			name: "fix iteration 1",
			children: []meithealv1alpha1.AgenticTask{
				child("review-641-0", succeeded, meithealv1alpha1.AgenticTaskVerdictNoGo),
				child("code-641-r1", succeeded, goVerdict),
				child("verify-641-r1", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
				issueAskUntrusted("review-641-0-r1", "REJECT"),
			},
			wantName: "review-641-0-r1-rr",
			wantDep:  "verify-641-r1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := iterationWorkload([]int32{641}, 1, nil)
			w.Spec.ReviewerAgentRefs[0].Name = "strict-reviewer"
			if tc.gateless {
				w.Spec.VerifierAgentRef = nil
			}
			plan := reviewIterationSteps(w, tc.children)
			if len(plan.steps) != 1 {
				t.Fatalf("steps = %v, want exactly the re-review", plan.steps)
			}
			s := plan.steps[0]
			if s.Name != tc.wantName {
				t.Fatalf("name = %q, want %q", s.Name, tc.wantName)
			}
			if s.Kind != meithealv1alpha1.AgenticTaskKindReview || s.AgentRef.Name != "strict-reviewer" {
				t.Fatalf("kind/agent = %s/%s, want a review by the same reviewer Agent", s.Kind, s.AgentRef.Name)
			}
			if s.Payload.Branch != "meitheal/wl/issue-641" || s.Payload.Issue != 641 || !s.Payload.OpenPullRequest {
				t.Fatalf("payload = %+v, want the issue branch with openPullRequest", s.Payload)
			}
			if len(s.DependsOn) != 1 || s.DependsOn[0] != tc.wantDep {
				t.Fatalf("dependsOn = %v, want [%s]", s.DependsOn, tc.wantDep)
			}
			if len(plan.rerunOf) != 1 || plan.rerunOf[0].reason != demotionReasonIssueAsk {
				t.Fatalf("rerunOf = %+v, want the untrusted review and its rail reason", plan.rerunOf)
			}
		})
	}
}

// TestUntrustedFindingsNeverReachTheCoder: the coder prompt carries only
// trusted feedback. Before #41 an untrusted NO-GO's findings about files the
// branch never touched became the coder's task list.
func TestUntrustedFindingsNeverReachTheCoder(t *testing.T) {
	succeeded := meithealv1alpha1.AgenticTaskPhaseSucceeded
	goVerdict := meithealv1alpha1.AgenticTaskVerdictGo
	cases := map[string][]meithealv1alpha1.AgenticTask{
		"trusted re-review NO-GO": {
			child("code-641", succeeded, goVerdict),
			child("verify-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
			issueAskUntrusted(reviewStep, "REJECT"),
			withStep(noGoChild("the retry path is untested",
				`[{"severity":"major","area":"tests","message":"no test covers the retry"}]`), "review-641-0-rr"),
		},
		"trusted NO-GO beside an untrusted one": {
			child("code-641", succeeded, goVerdict),
			child("verify-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
			withStep(noGoChild("the retry path is untested",
				`[{"severity":"major","area":"tests","message":"no test covers the retry"}]`), "review-641-0"),
			issueAskUntrusted("review-641-1", "REJECT"),
		},
	}
	for name, children := range cases {
		t.Run(name, func(t *testing.T) {
			plan := reviewIterationSteps(iterationWorkload([]int32{641}, 2, nil), children)
			var prompt string
			for _, s := range plan.steps {
				if s.Kind == meithealv1alpha1.AgenticTaskKindIssueFix {
					prompt = s.Payload.Prompt
				}
			}
			if !strings.Contains(prompt, "no test covers the retry") {
				t.Fatalf("prompt must carry the trusted feedback:\n%s", prompt)
			}
			if strings.Contains(prompt, "ghost.go") {
				t.Fatalf("prompt must not carry the untrusted review's findings:\n%s", prompt)
			}
		})
	}
}

// TestActiveChildren_RereviewSupersedesReview: once the re-review exists, the
// untrusted NO-GO it re-runs no longer counts, so a trusted GO re-review
// completes the Workload and the escalation trigger judges the re-review.
func TestActiveChildren_RereviewSupersedesReview(t *testing.T) {
	succeeded := meithealv1alpha1.AgenticTaskPhaseSucceeded
	w := iterationWorkload([]int32{641}, 2, nil)
	children := []meithealv1alpha1.AgenticTask{
		child("code-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
		child("verify-641", succeeded, meithealv1alpha1.AgenticTaskVerdictGatePass),
		child("review-641-0", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
		issueAskUntrusted("review-641-1", "REJECT"),
		child("review-641-1-rr", succeeded, meithealv1alpha1.AgenticTaskVerdictGo),
	}
	active := activeChildren(w, children)
	names := make([]string, 0, len(active))
	for i := range active {
		names = append(names, active[i].Labels[labelStep])
	}
	want := "code-641,verify-641,review-641-0,review-641-1-rr"
	if strings.Join(names, ",") != want {
		t.Fatalf("active = %v, want %s", names, want)
	}
	if c := classifyChildren(active); c.incomplete != 0 || c.succeeded != 4 {
		t.Fatalf("rollup buckets = %+v, want 4 on-target and nothing incomplete", c)
	}
}
