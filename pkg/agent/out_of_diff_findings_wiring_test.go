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

package agent_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	meithealagent "github.com/defilantech/meitheal/pkg/agent"
	"github.com/defilantech/meitheal/pkg/agent/reviewer"
)

// Production-path tests for the #41 out-of-diff rail and for the result
// shapes the controller's untrusted-review predicate (untrustedNoGo,
// internal/controller/workload_iteration.go) reads. Each drives the REAL
// rails through Execute -> runLLMPath -> the reviewer block. The branch
// touches only fix.go (csPushNonEmptyBranch); the reviewer's findings cite a
// file the branch never touched, the #41 failure.
//
// Mutation check: remove the enforceReviewerOutOfDiffFindings call from
// runLLMPath and TestOutOfDiffRail_WiredIntoRunLLMPath fails, because a
// REJECT is exempt from grounded demotion and nothing else stamps
// verdictDemotedBy on a verified NO-GO.

// submitNoGo is the model turn that calls submit_result; the terminal result
// the loop records comes from the fakeRegistry entry for submit_result.
const submitNoGo = `{
  "id": "t-submit",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "tool_calls": [{
        "id": "tc-submit",
        "type": "function",
        "function": {"name": "submit_result", "arguments": "{\"verdict\":\"NO-GO\",\"summary\":\"REJECT\"}"}
      }]
    },
    "finish_reason": "tool_calls"
  }]
}`

// ghostFinding cites a file that is not in the diff (nor in the repository).
var ghostFinding = map[string]any{
	"severity": "blocker",
	"area":     "scope",
	"message":  "the controller still drops the retry budget",
	"file":     "internal/controller/ghost.go",
	"line":     42,
}

// runOutOfDiffReview executes one reviewer task whose fetched issue body is
// issueBody and whose submit_result extra is extra, and returns modelExtra.
func runOutOfDiffReview(
	t *testing.T, issueBody string, extra map[string]any,
) (meithealv1alpha1.AgenticTaskVerdict, map[string]any) {
	t.Helper()
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	const branch = "meitheal/review-out-of-diff"
	csPushNonEmptyBranch(t, root, bare, branch)
	oaiSrv := scriptedOAI(t, []string{fetchIssueBody, submitNoGo})

	reg := &fakeRegistry{results: map[string]*meithealagent.ToolResult{
		"fetch_issue": {Output: map[string]any{"body": issueBody}},
		"submit_result": {
			Terminal: true,
			Verdict:  "NO-GO",
			Summary:  "REJECT: the change misses the controller path",
			Extra:    extra,
		},
	}}
	e := reviewExecutor(t, bare, branch, reg, oaiSrv)

	res, err := execWithAgent(t, e, &meithealv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "review-demo", Namespace: "default", UID: types.UID("review-demo-uid")},
		Spec: meithealv1alpha1.AgenticTaskSpec{
			Kind: meithealv1alpha1.AgenticTaskKindReview,
			Payload: meithealv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  1678,
				Branch: branch,
			},
			AgentRef: &corev1.LocalObjectReference{Name: "reviewer-demo"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	me, ok := res.Extra["modelExtra"].(map[string]any)
	if !ok {
		t.Fatalf("modelExtra missing or wrong type: %T (%v)", res.Extra["modelExtra"], res.Extra)
	}
	return res.Verdict, me
}

// TestOutOfDiffRail_WiredIntoRunLLMPath: a REJECT (exempt from grounded
// demotion) whose only blocker cites a file outside the diff, with a verified
// issueAsk, comes out as the reviewer's NO-GO marked untrusted by the
// out-of-diff rail.
func TestOutOfDiffRail_WiredIntoRunLLMPath(t *testing.T) {
	verdict, me := runOutOfDiffReview(t,
		"## Bug\n\nThe retry budget is dropped on reconnect.",
		map[string]any{
			"reviewOutcome": "REJECT",
			// Verbatim substring of the body, so issueAsk verifies and its
			// rail leaves the markers alone.
			"issueAsk": "The retry budget is dropped on reconnect.",
			"findings": []any{ghostFinding},
		})
	if verdict != meithealv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("verdict = %s, want the reviewer's NO-GO kept", verdict)
	}
	if me["issueAskVerified"] != true {
		t.Fatalf("fixture drift: issueAsk must verify so only the out-of-diff rail acts, got %v", me["issueAskVerified"])
	}
	if got := me["verdictDemotedBy"]; got != reviewer.RailOutOfDiffFindings {
		t.Fatalf("verdictDemotedBy = %v, want %q", got, reviewer.RailOutOfDiffFindings)
	}
	if got := me["verdictClaimed"]; got != string(meithealv1alpha1.AgenticTaskVerdictNoGo) {
		t.Fatalf("verdictClaimed = %v, want NO-GO (the reviewer's own verdict)", got)
	}
	if got := me["verdictDemoted"]; got != true {
		t.Fatalf("verdictDemoted = %v, want true", got)
	}
}

// TestOutOfDiffRail_IssueNamedFileStaysTrusted: a wrong-scope REJECT points
// at the file the issue names, which the diff does not touch. That is the
// rejection doing its job, so the review stays trusted.
func TestOutOfDiffRail_IssueNamedFileStaysTrusted(t *testing.T) {
	verdict, me := runOutOfDiffReview(t,
		"## Bug\n\nEdit internal/controller/ghost.go so the retry budget survives.",
		map[string]any{
			"reviewOutcome": "REJECT",
			"issueAsk":      "Edit internal/controller/ghost.go so the retry budget survives.",
			"findings":      []any{ghostFinding},
		})
	if verdict != meithealv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("verdict = %s, want NO-GO", verdict)
	}
	if by, ok := me["verdictDemotedBy"]; ok {
		t.Fatalf("a finding citing a file the issue names must stay trusted, got verdictDemotedBy=%v", by)
	}
}

// TestUngroundedNoGoThenUnverifiedIssueAsk_Shape pins the producer side of
// the controller's third untrusted case. A REQUEST-CHANGES whose blocker
// cites a file outside the diff is demoted to GO by the grounded-finding
// rail, and an unverified issueAsk then rewrites that GO back to NO-GO. The
// result reads as an issueAsk GO->NO-GO rewrite carrying findings, which the
// controller used to iterate on, handing the coder the fabricated finding.
func TestUngroundedNoGoThenUnverifiedIssueAsk_Shape(t *testing.T) {
	verdict, me := runOutOfDiffReview(t,
		"## Bug\n\nThe retry budget is dropped on reconnect.",
		map[string]any{
			"reviewOutcome": "REQUEST-CHANGES",
			// Not in the body, so reconcileReviewerIssueAsk marks it unverified.
			"issueAsk": "make the scheduler faster",
			"findings": []any{ghostFinding},
		})
	if verdict != meithealv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("verdict = %s, want NO-GO from the issueAsk rail", verdict)
	}
	if got := me["verdictDemotedBy"]; got != reviewer.RailIssueAsk {
		t.Fatalf("verdictDemotedBy = %v, want %q", got, reviewer.RailIssueAsk)
	}
	if got := me["verdictClaimed"]; got != string(meithealv1alpha1.AgenticTaskVerdictGo) {
		t.Fatalf("verdictClaimed = %v, want GO (the verdict the issueAsk rail saw)", got)
	}
	if got := me["groundedFindingDemotion"]; got != true {
		t.Fatalf("groundedFindingDemotion = %v, want true: the controller keys the untrusted case on it", got)
	}
	findings, _ := reviewer.ParseFindings(me)
	if len(findings) == 0 {
		t.Fatalf("findings must survive the demotions (they are what the coder would be handed), got %v", me["findings"])
	}
}
