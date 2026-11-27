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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent/reviewer"
)

// conditionTypeReviewIterationTriggered reports that at least one
// issue's reviewer round went terminal with a NO-GO and a bounded fix
// iteration was appended (#946): a new coder task carrying the review
// feedback in payload.prompt, plus a fresh verify + reviewer fan-out
// chained behind it.
const conditionTypeReviewIterationTriggered = "ReviewIterationTriggered"

// conditionTypeReviewIterationSuppressed reports that a terminal NO-GO did
// NOT open a fix iteration because the issueAsk rail manufactured it out of
// the reviewer's own GO and it carries no findings (#1636). The demoted
// NO-GO still counts as incomplete in the rollup, so without this the
// operator sees a Failed Workload on a branch the reviewer approved and
// nothing anywhere says why no iteration ran.
const conditionTypeReviewIterationSuppressed = "ReviewIterationSuppressed"

// conditionTypeReviewRerunTriggered reports that a reviewer NO-GO a harness
// rail marked untrusted did NOT open a fix iteration and was re-reviewed once
// on the same branch instead (#41). The coder cannot act on a rejection the
// harness itself does not believe: in #41 the reviewer's blockers cited files
// absent from the branch, the coder said so, and the Workload failed.
const conditionTypeReviewRerunTriggered = "ReviewRerunTriggered"

// conditionTypeReviewUntrusted reports that a re-review came back untrusted
// as well, so the issue ends incomplete without a fix iteration (#41). The
// untrusted NO-GO still rolls the Workload to Failed, and still fires the
// escalation reviewers when they are configured; this condition is what
// tells the operator the failure is the review's trustworthiness, not the
// branch.
const conditionTypeReviewUntrusted = "ReviewUntrusted"

// rereviewSuffix marks the single re-run of a review whose NO-GO was
// untrusted (#41): review-<n>-<i>-rr in the base round,
// review-<n>-<i>-r<k>-rr in fix iteration k. It belongs to the same round as
// the review it re-runs, so parseReviewStep reports that round's k.
const rereviewSuffix = "-rr"

// defaultMaxReviewIterations is the per-issue fix-iteration bound when
// Workload.spec.maxReviewIterations is unset. One iteration mirrors
// what a human does with a single request-changes round; an explicit 0
// opts back into the pre-#946 fail-on-first-NO-GO behavior.
const defaultMaxReviewIterations = 1

// effectiveMaxReviewIterations resolves the *int32 three-state:
// nil defaults, explicit values (including 0) win.
func effectiveMaxReviewIterations(w *meithealv1alpha1.Workload) int {
	if w.Spec.MaxReviewIterations == nil {
		return defaultMaxReviewIterations
	}
	return int(*w.Spec.MaxReviewIterations)
}

// iterationSuffixed renders the step name for fix iteration k: the
// base name for the initial round (k=0), "<base>-r<k>" for k >= 1.
func iterationSuffixed(base string, k int) string {
	if k == 0 {
		return base
	}
	return fmt.Sprintf("%s-r%d", base, k)
}

func codeStepName(n int32, k int) string {
	return iterationSuffixed(fmt.Sprintf("code-%d", n), k)
}

func verifyStepName(n int32, k int) string {
	return iterationSuffixed(fmt.Sprintf("verify-%d", n), k)
}

func reviewStepName(n int32, i, k int) string {
	return iterationSuffixed(fmt.Sprintf("review-%d-%d", n, i), k)
}

// parseIterationTail matches "<base>" (iteration 0) or "<base>-r<k>"
// (iteration k >= 1). Returns ok=false for anything else, including
// longer names that merely share the base as a prefix (code-64 must
// not match code-641).
func parseIterationTail(step, base string) (int, bool) {
	if step == base {
		return 0, true
	}
	tail, found := strings.CutPrefix(step, base+"-r")
	if !found {
		return 0, false
	}
	k, err := strconv.Atoi(tail)
	if err != nil || k < 1 {
		return 0, false
	}
	return k, true
}

// rereviewStepName renders the re-review of reviewer i in fix iteration k
// (#41).
func rereviewStepName(n int32, i, k int) string {
	return reviewStepName(n, i, k) + rereviewSuffix
}

// parseReviewStep parses a reviewer fan-out step name for issue n:
// "review-<n>-<i>" or "review-<n>-<i>-r<k>", each optionally followed by the
// re-review suffix "-rr" (#41). It returns the reviewer index, the fix
// iteration, and whether the step is a re-review. The reviewer index digits
// are validated so issue 64 never cross-matches review-641-0, and the
// coder-escalation reviews (review-<n>-esc-<i>) never parse.
func parseReviewStep(step string, n int32) (idx, k int, rereview, ok bool) {
	rest, found := strings.CutPrefix(step, fmt.Sprintf("review-%d-", n))
	if !found {
		return 0, 0, false, false
	}
	rest, rereview = strings.CutSuffix(rest, rereviewSuffix)
	idxStr, tail, hasIter := strings.Cut(rest, "-r")
	if idxStr == "" {
		return 0, 0, false, false
	}
	for _, c := range idxStr {
		if c < '0' || c > '9' {
			return 0, 0, false, false
		}
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return 0, 0, false, false
	}
	if !hasIter {
		return idx, 0, rereview, true
	}
	k, err = strconv.Atoi(tail)
	if err != nil || k < 1 {
		return 0, 0, false, false
	}
	return idx, k, rereview, true
}

// reviewIterationOf parses a reviewer fan-out step name for issue n
// ("review-<n>-<i>" or "review-<n>-<i>-r<k>", or either one's re-review)
// and returns its fix iteration.
func reviewIterationOf(step string, n int32) (int, bool) {
	_, k, _, ok := parseReviewStep(step, n)
	return k, ok
}

// issueStepIteration classifies a synthesized issue-batch step name
// (code / verify / review, any iteration) for issue n. Escalation
// steps and foreign names return ok=false.
func issueStepIteration(step string, n int32) (int, bool) {
	if k, ok := parseIterationTail(step, fmt.Sprintf("code-%d", n)); ok {
		return k, true
	}
	if k, ok := parseIterationTail(step, fmt.Sprintf("verify-%d", n)); ok {
		return k, true
	}
	return reviewIterationOf(step, n)
}

// suppressedDemotion records one review child whose inert demotion kept a
// fix iteration from opening, so emitReviewIterations can say so. Without
// it the demoted NO-GO still lands in the incomplete bucket
// (classifyChildren, workload_controller.go) and rolls the Workload to
// Failed on a branch the reviewer approved, with nothing anywhere
// explaining why no iteration ran (#1636).
type suppressedDemotion struct {
	// step is the review child's step label (e.g. review-641-0), falling
	// back to its object name when the label is missing.
	step string
	// reason is the rail's demotionReason, verbatim.
	reason string
}

// taskStepLabel names a child for operator-facing prose: its step label
// when it carries one, its object name otherwise.
func taskStepLabel(t *meithealv1alpha1.AgenticTask) string {
	if label := t.Labels[labelStep]; label != "" {
		return label
	}
	return t.Name
}

// inertDemotion reports whether a NO-GO was manufactured by a harness rail
// rather than asserted by the reviewer, AND carries nothing to act on
// (#1636). It returns the rail's demotionReason so the caller can record
// why the fix iteration did not run.
//
// enforceReviewerIssueAsk (pkg/agent/executor_native.go) rewrites a
// reviewer's GO to NO-GO when it cannot verify that the stated issue ask
// covers the fetched issue body. That is a statement about verification
// confidence, not about the change. Treating it as a rejection opens a fix
// iteration whose feedback body is the reviewer's own approval followed by
// an empty findings list, and re-running the coder cannot make an
// unverifiable issueAsk verify, so the loop has no terminating condition.
//
// Three conditions are required, and each one excludes a NO-GO the coder
// CAN act on:
//
//   - verdictDemotedBy is the issueAsk, scope-overlap, or unverified-summary
//     rail. A rail that rewrites a GO without findings manufactures a
//     rejection the coder cannot act on: re-running cannot make an
//     unverifiable issueAsk verify, an issue that names the symptom file
//     rather than the fix site is scope drift the coder already answered by
//     fixing the cause (#1684), and a GO whose own summary admits
//     verification could not be performed (#1454) is a statement about the
//     review environment, which no re-run changes either. Keying off the
//     bare verdictDemoted flag alone would not distinguish these rails from
//     each other, but all are inert when they carry no findings, so the
//     single condition below accepts any of their names.
//   - verdictClaimed is GO, i.e. the rail really did rewrite the verdict.
//     enforceReviewerIssueAsk stamps the demotion fields on a path where
//     it does NOT: an unverified non-GO review is marked untrusted and the
//     reviewer's OWN NO-GO is returned. Suppressing that would discard a
//     rejection that may be genuine; untrustedNoGo routes it to one
//     re-review instead (#41).
//   - no findings. A demotion carrying findings is real work either way.
//
// Every field is read from extra.modelExtra. The executor's
// modelDecidedResult nests the rails' whole map there and
// promoteTerminalOutcome lifts only outcome / unverified / resolvedBy to
// the top level, so reading these keys from extra directly (as this
// predicate first shipped) never matched anything the executor emits.
func inertDemotion(t *meithealv1alpha1.AgenticTask) (reason string, inert bool) {
	modelExtra, ok := reviewModelExtra(t)
	// Malformed results are not our call to suppress; let them iterate.
	if !ok {
		return "", false
	}
	if by, _ := modelExtra["verdictDemotedBy"].(string); by != reviewer.RailIssueAsk &&
		by != reviewer.RailScopeOverlap && by != reviewer.RailUnverifiedSummary {
		return "", false
	}
	if claimed, _ := modelExtra["verdictClaimed"].(string); claimed != string(meithealv1alpha1.AgenticTaskVerdictGo) {
		return "", false
	}
	if findings, _ := reviewer.ParseFindings(modelExtra); len(findings) > 0 {
		return "", false
	}
	// Mirror reviewFeedbackSection's fallback: a non-conforming findings
	// shape still reaches the coder as raw JSON, so it is not inert.
	if raw, ok := modelExtra["findings"]; ok {
		if buf, err := json.Marshal(raw); err == nil {
			switch string(buf) {
			case "null", "[]", "{}", "":
			default:
				return "", false
			}
		}
	}
	reason, _ = modelExtra["demotionReason"].(string)
	return reason, true
}

// reviewModelExtra decodes a review task's extra.modelExtra, where the
// executor's modelDecidedResult nests the rails' whole map. ok is false for a
// result-less task or a malformed result.
func reviewModelExtra(t *meithealv1alpha1.AgenticTask) (map[string]any, bool) {
	if t.Status.Result == nil || len(t.Status.Result.Raw) == 0 {
		return nil, false
	}
	var envelope struct {
		Extra struct {
			ModelExtra map[string]any `json:"modelExtra"`
		} `json:"extra"`
	}
	if err := json.Unmarshal(t.Status.Result.Raw, &envelope); err != nil {
		return nil, false
	}
	return envelope.Extra.ModelExtra, true
}

// untrustedNoGo reports whether a NO-GO is a rejection the executor's rails
// do not believe, so a fix iteration would hand the coder findings it cannot
// act on (#41). It returns the rails' reasons for the operator. Callers check
// inertDemotion first: an empty-handed GO->NO-GO rewrite is suppressed
// outright, and only a NO-GO that still carries the reviewer's rejection
// reaches this predicate.
//
// Three result shapes qualify, each read from extra.modelExtra:
//
//   - verdictDemotedBy is the out-of-diff rail: every blocking finding cites
//     a file the branch diff does not touch.
//   - verdictDemotedBy is the issueAsk rail and verdictClaimed is not GO: the
//     rail could not verify the reviewer read the issue and kept the
//     reviewer's own NO-GO, marked untrusted (enforceReviewerIssueAsk).
//   - verdictDemotedBy is the issueAsk rail, verdictClaimed is GO, and
//     groundedFindingDemotion is set: the reviewer's own NO-GO was demoted to
//     GO because no blocking finding cites a changed line, and the issueAsk
//     rail then rewrote that GO back to NO-GO. The NO-GO carries the very
//     findings the grounded-finding rail rejected.
//
// A scope-overlap or unverified-summary demotion carrying findings is not
// here: scope drift is computed from the diff and the issue, which the coder
// can act on, and the summary rail fires on a GO, whose findings are the
// reviewer's own.
func untrustedNoGo(t *meithealv1alpha1.AgenticTask) (reason string, untrusted bool) {
	modelExtra, ok := reviewModelExtra(t)
	if !ok {
		return "", false
	}
	by, _ := modelExtra["verdictDemotedBy"].(string)
	claimed, _ := modelExtra["verdictClaimed"].(string)
	reason, _ = modelExtra["demotionReason"].(string)
	switch {
	case by == reviewer.RailOutOfDiffFindings:
	case by == reviewer.RailIssueAsk && claimed != string(meithealv1alpha1.AgenticTaskVerdictGo):
	case by == reviewer.RailIssueAsk && modelExtra["groundedFindingDemotion"] == true:
		if grounded, _ := modelExtra["groundedFindingReason"].(string); grounded != "" {
			reason = joinReasons(reason, grounded)
		}
	default:
		return "", false
	}
	return reason, true
}

// joinReasons joins the non-empty reasons with "; ".
func joinReasons(reasons ...string) string {
	kept := reasons[:0:0]
	for _, r := range reasons {
		if r != "" {
			kept = append(kept, r)
		}
	}
	return strings.Join(kept, "; ")
}

// reviewRound is what one reviewer round of one issue decided, once every
// review in it is terminal.
type reviewRound struct {
	// noGo are the trusted NO-GO reviews: the round opens a fix iteration
	// whose coder prompt carries their feedback.
	noGo []*meithealv1alpha1.AgenticTask
	// inert are the NO-GOs a rail manufactured out of a GO with nothing to
	// act on (#1636); they neither iterate nor re-review.
	inert []suppressedDemotion
	// rereview lists the reviewers whose NO-GO is untrusted and has not
	// been re-run yet (#41).
	rereview []rereviewDue
	// untrusted are re-reviews whose NO-GO is untrusted again; the round
	// ends there (#41).
	untrusted []suppressedDemotion
}

// rereviewDue is one untrusted NO-GO due for its re-review (#41).
type rereviewDue struct {
	// idx is the reviewer's index in spec.reviewerAgentRefs.
	idx int
	// review names the untrusted review and the rails' reasons.
	review suppressedDemotion
}

// reviewRoundOutcome inspects issue n's reviewer fan-out for fix iteration k.
// A round is decided once at least one review child exists and every one is
// terminal (Phase Succeeded or Failed); an undecided round returns the zero
// value, because an in-flight review may still produce a NO-GO that iterates.
// A cascade INCOMPLETE after GATE-FAIL (#548) is terminal but not a NO-GO, so
// a branch the gate rejected never triggers a fix iteration. Scanning what
// exists (rather than the expected reviewer count) keeps cloud-suppressed
// reviewers from permanently blocking the trigger, mirroring escalationSteps.
//
// A reviewer's re-review (#41) stands in for its original review: once
// review-<n>-<i>[-r<k>]-rr exists, the round waits on it and judges that
// reviewer by it alone. Each NO-GO then lands in exactly one bucket:
//   - inert (#1636): a rail's empty-handed GO->NO-GO rewrite;
//   - untrusted (#41) on an original review: due for its one re-review;
//   - untrusted on a re-review: the round ends without an iteration;
//   - anything else: a trusted NO-GO that iterates.
func reviewRoundOutcome(children []meithealv1alpha1.AgenticTask, n int32, k int) reviewRound {
	originals := map[int]*meithealv1alpha1.AgenticTask{}
	reruns := map[int]*meithealv1alpha1.AgenticTask{}
	var indices []int
	for i := range children {
		idx, iter, rereview, ok := parseReviewStep(children[i].Labels[labelStep], n)
		if !ok || iter != k {
			continue
		}
		if _, seen := originals[idx]; !seen {
			if _, seen := reruns[idx]; !seen {
				indices = append(indices, idx)
			}
		}
		if rereview {
			reruns[idx] = &children[i]
		} else {
			originals[idx] = &children[i]
		}
	}
	if len(indices) == 0 {
		return reviewRound{}
	}
	sort.Ints(indices)

	judged := make([]*meithealv1alpha1.AgenticTask, 0, len(indices))
	for _, idx := range indices {
		t := originals[idx]
		if rerun, ok := reruns[idx]; ok {
			t = rerun
		}
		if t.Status.Phase != meithealv1alpha1.AgenticTaskPhaseSucceeded &&
			t.Status.Phase != meithealv1alpha1.AgenticTaskPhaseFailed {
			return reviewRound{}
		}
		judged = append(judged, t)
	}

	var round reviewRound
	for j, t := range judged {
		if t.Status.Verdict != meithealv1alpha1.AgenticTaskVerdictNoGo {
			continue
		}
		if reason, inert := inertDemotion(t); inert {
			round.inert = append(round.inert, suppressedDemotion{step: taskStepLabel(t), reason: reason})
			continue
		}
		reason, untrusted := untrustedNoGo(t)
		if !untrusted {
			round.noGo = append(round.noGo, t)
			continue
		}
		if _, rerun := reruns[indices[j]]; rerun {
			round.untrusted = append(round.untrusted, suppressedDemotion{step: taskStepLabel(t), reason: reason})
			continue
		}
		round.rereview = append(round.rereview, rereviewDue{
			idx:    indices[j],
			review: suppressedDemotion{step: taskStepLabel(t), reason: reason},
		})
	}
	return round
}

// iterationPlan is what reviewIterationSteps decided for a Workload.
type iterationPlan struct {
	// steps are the code / verify / review steps to emit now.
	steps []meithealv1alpha1.PipelineStep
	// iterated are the issues a fix iteration step was emitted for.
	iterated []int32
	// rereviewed are the issues a re-review step was emitted for (#41).
	rereviewed []int32
	// rerunOf are the untrusted reviews those re-review steps re-run.
	rerunOf []suppressedDemotion
	// suppressed are the inert demotions that kept a round from iterating
	// (#1636); see suppressedDemotion.
	suppressed []suppressedDemotion
	// untrusted are the re-reviews whose NO-GO was untrusted again, so the
	// round ended without an iteration (#41).
	untrusted []suppressedDemotion
}

// reviewIterationSteps decides which code-<n>-r<k> / verify-<n>-r<k> /
// review-<n>-<i>-r<k> steps, and which re-reviews, to emit NOW, given the
// Workload spec and its current children (#946, #41).
//
// For each issue n it walks the reviewer rounds in order. Round k
// (0..maxReviewIterations) opens fix iteration k+1 once it is decided with
// at least one trusted NO-GO and the budget has room. Steps whose child
// already exists are skipped individually, so a partial create failure is
// repaired on the next reconcile instead of stranding the tail of the
// iteration — the same idempotency-by-existence contract as
// escalationSteps. The walk stops at the first round that is still in
// flight, converged (no trusted NO-GO), or the last one in the budget, so
// exhausting maxReviewIterations leaves the terminal NO-GO for rollup to
// fail on as before #946.
//
// A round whose only NO-GOs are untrusted (#41) does not iterate. Each such
// reviewer is re-run once on the same branch as review-<n>-<i>[-r<k>]-rr,
// in any round including the last, because the question is whether to
// believe the rejection, not whether the coder gets another attempt. A
// trusted re-review then decides the round as any review does; an untrusted
// one ends it. A round that has a trusted NO-GO as well iterates on that
// one alone and re-runs nothing: the iteration brings a fresh review round
// anyway.
//
// The iteration's coder step re-targets the SAME branch: the payload
// sets reviseFromBranch to the task's own branch name so the executor
// restores the prior attempt from the push remote before the model
// runs (#951), allowOverwrite so the coder can replace that attempt
// (force-with-lease push, #573/#934), and payload.prompt carries the
// distilled review feedback so the retry is not blind. The coder step
// references spec.revisionCoderAgentRef when set — a revision amends
// existing work and wants a revision-tuned profile, not the issue-fix
// forcing profile — and falls back to spec.coderAgentRef otherwise
// (the caller emits the RevisionUnderIssueFixProfile warning).
//
// Pure function: no API calls, no status writes. The caller owns
// MaxTasks accounting, sovereignty filtering, and creation, and must
// restrict invocation to issue-batch mode (user-authored Pipeline step
// names could false-match the synthesized naming scheme).
func reviewIterationSteps(
	w *meithealv1alpha1.Workload, children []meithealv1alpha1.AgenticTask,
) iterationPlan {
	var plan iterationPlan
	maxIter := effectiveMaxReviewIterations(w)
	if maxIter <= 0 ||
		len(w.Spec.ReviewerAgentRefs) == 0 ||
		len(w.Spec.Issues) == 0 ||
		w.Spec.CoderAgentRef == nil {
		return plan
	}

	existing := make(map[string]struct{}, len(children))
	for i := range children {
		if step := children[i].Labels[labelStep]; step != "" {
			existing[step] = struct{}{}
		}
	}

	for _, n := range w.Spec.Issues {
		issueIterated, issueRereviewed := false, false
		for k := 0; k <= maxIter; k++ {
			round := reviewRoundOutcome(children, n, k)
			plan.suppressed = append(plan.suppressed, round.inert...)
			plan.untrusted = append(plan.untrusted, round.untrusted...)
			if len(round.noGo) == 0 {
				for _, due := range round.rereview {
					if step, ok := rereviewStep(w, n, due.idx, k, existing); ok {
						plan.steps = append(plan.steps, step)
						plan.rerunOf = append(plan.rerunOf, due.review)
						issueRereviewed = true
					}
				}
				break
			}
			if k == maxIter {
				break
			}
			steps := fixIterationSteps(w, n, k+1, round.noGo, existing)
			plan.steps = append(plan.steps, steps...)
			issueIterated = issueIterated || len(steps) > 0
		}
		if issueIterated {
			plan.iterated = append(plan.iterated, n)
		}
		if issueRereviewed {
			plan.rereviewed = append(plan.rereviewed, n)
		}
	}
	return plan
}

// reviewDependency names the step fix iteration k's reviews hang off: its
// verify step, or its code step on a gateless Workload (no
// VerifierAgentRef), mirroring the base round's shape.
func reviewDependency(w *meithealv1alpha1.Workload, n int32, k int) string {
	if w.Spec.VerifierAgentRef != nil {
		return verifyStepName(n, k)
	}
	return codeStepName(n, k)
}

// reviewStepFor renders reviewer i's review step for issue n under name,
// hanging off fix iteration k's gate.
func reviewStepFor(w *meithealv1alpha1.Workload, n int32, i, k int, name string) meithealv1alpha1.PipelineStep {
	// Mirrors the base-round stamp (#937): an iterated-then-approved issue
	// must still open its PR.
	openPR := w.Spec.OpenPullRequest == nil || *w.Spec.OpenPullRequest
	return meithealv1alpha1.PipelineStep{
		Name:      name,
		Kind:      meithealv1alpha1.AgenticTaskKindReview,
		AgentRef:  w.Spec.ReviewerAgentRefs[i],
		DependsOn: []string{reviewDependency(w, n, k)},
		Payload: meithealv1alpha1.AgenticTaskPayload{
			Repo:            w.Spec.Repo,
			Issue:           n,
			Branch:          issueBranch(w, n),
			OpenPullRequest: openPR,
		},
	}
}

// rereviewStep renders the one re-review of reviewer i in round k (#41): a
// fresh review task by the same reviewer Agent on the same branch, behind
// the same (already terminal) gate. ok is false when the step already exists
// or the reviewer index no longer names a configured reviewer.
func rereviewStep(
	w *meithealv1alpha1.Workload, n int32, i, k int, existing map[string]struct{},
) (meithealv1alpha1.PipelineStep, bool) {
	name := rereviewStepName(n, i, k)
	if _, ok := existing[name]; ok || i >= len(w.Spec.ReviewerAgentRefs) {
		return meithealv1alpha1.PipelineStep{}, false
	}
	return reviewStepFor(w, n, i, k, name), true
}

// issueBranch is the branch every synthesized step for issue n works on.
func issueBranch(w *meithealv1alpha1.Workload, n int32) string {
	return fmt.Sprintf("meitheal/%s/issue-%d", w.Name, n)
}

// fixIterationSteps renders fix iteration k for issue n, skipping the steps
// that already exist: the coder step carrying noGo's feedback, the verify
// step when the Workload is gated, and a fresh review per reviewer.
func fixIterationSteps(
	w *meithealv1alpha1.Workload, n int32, k int,
	noGo []*meithealv1alpha1.AgenticTask, existing map[string]struct{},
) []meithealv1alpha1.PipelineStep {
	coderRef := w.Spec.CoderAgentRef
	if w.Spec.RevisionCoderAgentRef != nil {
		coderRef = w.Spec.RevisionCoderAgentRef
	}
	branch := issueBranch(w, n)
	codeName := codeStepName(n, k)
	verifyName := verifyStepName(n, k)

	var steps []meithealv1alpha1.PipelineStep
	if _, ok := existing[codeName]; !ok {
		steps = append(steps, meithealv1alpha1.PipelineStep{
			Name:     codeName,
			Kind:     meithealv1alpha1.AgenticTaskKindIssueFix,
			AgentRef: *coderRef,
			Payload: meithealv1alpha1.AgenticTaskPayload{
				Repo:   w.Spec.Repo,
				Issue:  n,
				Branch: branch,
				// The prior attempt lives at the task's own branch name on
				// the push remote; the executor restores it (#951) and, under
				// the rebase strategy, replays it onto the current base so
				// this in-review revision does not revert work merged since
				// the prior attempt (#1029).
				ReviseFromBranch: branch,
				BranchStrategy:   meithealv1alpha1.BranchStrategyRebase,
				AllowOverwrite:   true,
				Prompt:           reviewFeedbackPrompt(noGo),
				PromptPrefix:     intentPromptPrefix(w),
			},
		})
	}
	// Gateless Workloads (no VerifierAgentRef) skip the verify step; the
	// iteration's reviews hang directly off the fix commit.
	if w.Spec.VerifierAgentRef != nil {
		if _, ok := existing[verifyName]; !ok {
			steps = append(steps, meithealv1alpha1.PipelineStep{
				Name:      verifyName,
				Kind:      meithealv1alpha1.AgenticTaskKindVerify,
				AgentRef:  *w.Spec.VerifierAgentRef,
				DependsOn: []string{codeName},
				Payload: meithealv1alpha1.AgenticTaskPayload{
					Repo:   w.Spec.Repo,
					Issue:  n,
					Branch: branch,
				},
			})
		}
	}
	for i := range w.Spec.ReviewerAgentRefs {
		name := reviewStepName(n, i, k)
		if _, ok := existing[name]; ok {
			continue
		}
		steps = append(steps, reviewStepFor(w, n, i, k, name))
	}
	return steps
}

// reviewFeedbackPrompt distills the NO-GO reviewers' structured
// results into the fix iteration's coder prompt. The executor's
// buildUserPrompt drops this text into the "Issue context" slot of the
// issue-fix template, so the retry runs with the rejection in front of
// it instead of blind (#946: 4 production issues burned 3 attempts
// each reproducing the same rejected patch).
//
// The wording matches the executor's revise-from-branch restore
// (#951): because the step's payload stamps reviseFromBranch, the
// workspace really does start from the prior attempt's files, so the
// prompt directs the model to amend that work as a delta rather than
// rebuild the fix from scratch.
func reviewFeedbackPrompt(noGo []*meithealv1alpha1.AgenticTask) string {
	var b strings.Builder
	b.WriteString("The reviewer rejected the previous attempt on this issue (verdict NO-GO).\n")
	b.WriteString("Your workspace already contains that previous attempt: the working branch was\n")
	b.WriteString("restored from this task's branch on the remote, so its files and history are\n")
	b.WriteString("present. Do not rebuild the fix from scratch. Amend the existing work with the\n")
	b.WriteString("smallest changes that address every point below, then push to the same branch.\n")
	for _, t := range noGo {
		b.WriteString("\n")
		b.WriteString(reviewFeedbackSection(t))
	}
	b.WriteString("\nAddress this feedback.\n")
	return b.String()
}

// reviewFeedbackSection renders one NO-GO reviewer's summary +
// findings. The structured findings live in the review task's
// status.result at extra.modelExtra (the reviewer's submit_result
// extra; see pkg/agent/reviewer). Rendering is best-effort:
// findings that fail the strict schema fall back to their raw JSON so
// legacy map-shaped findings ("scope_creep" + "*_details" keys) still
// reach the coder, and a result-less task degrades to a one-liner.
func reviewFeedbackSection(t *meithealv1alpha1.AgenticTask) string {
	label := taskStepLabel(t)

	var envelope struct {
		Summary string `json:"summary"`
		Extra   struct {
			ModelExtra map[string]any `json:"modelExtra"`
		} `json:"extra"`
	}
	if t.Status.Result != nil && len(t.Status.Result.Raw) > 0 {
		// Best-effort: malformed JSON leaves the envelope zero-valued.
		_ = json.Unmarshal(t.Status.Result.Raw, &envelope)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Reviewer %s", label)
	if envelope.Summary != "" {
		fmt.Fprintf(&b, ": %s", envelope.Summary)
	}
	b.WriteString("\n")

	findings, _ := reviewer.ParseFindings(envelope.Extra.ModelExtra)
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(&b, "- [%s/%s] %s", f.Severity, f.Area, f.Message)
			if f.File != "" {
				fmt.Fprintf(&b, " (%s", f.File)
				if f.Line > 0 {
					fmt.Fprintf(&b, ":%d", f.Line)
				}
				b.WriteString(")")
			}
			if f.Suggestion != "" {
				fmt.Fprintf(&b, "; suggested fix: %s", f.Suggestion)
			}
			b.WriteString("\n")
		}
		return b.String()
	}

	// Schema-strict parse yielded nothing; pass raw findings through so
	// non-conforming shapes still inform the retry.
	if raw, ok := envelope.Extra.ModelExtra["findings"]; ok {
		if buf, err := json.Marshal(raw); err == nil && len(buf) > 0 && string(buf) != "null" {
			fmt.Fprintf(&b, "- findings: %s\n", buf)
		}
	}
	return b.String()
}

// activeChildren filters out children that a later attempt superseded,
// so rollup and the escalation trigger judge each issue by its LATEST
// attempt only. Two supersession rules apply:
//
//   - Fix iteration (#946/#959): a later code-<n>-r<k> round supersedes
//     earlier synthesized steps for issue n. Without this, iteration 0's
//     terminal NO-GO review would keep IncompleteTasks non-zero forever
//     and a converged r1 round could never complete the Workload.
//   - Coder escalation (#963): once a code-<n>-esc task exists, the base
//     attempt's steps (code-<n>, verify-<n>, review-<n>-<i>, and any
//     iteration rounds) are dropped so the base coder's terminal NO-GO
//     and the cascade-failed verify/review it stranded do not pin the
//     Workload at Failed. The issue is then judged by the ESC attempt.
//   - Re-review (#41): once review-<n>-<i>[-r<k>]-rr exists, the review it
//     re-runs is dropped, so the untrusted NO-GO neither pins the rollup at
//     Failed after a trusted re-review approved nor fires the escalation
//     reviewers on its own. The re-review is only emitted once the review
//     it replaces is terminal.
//
// A superseded iteration round is by construction fully terminal (the
// k+1 trigger requires it); an escalated base attempt is likewise
// terminal (the escalation trigger requires a terminal base code task),
// so nothing in flight is ever hidden from rollup. The escalation steps
// themselves (suffix -esc, which do not parse as base issue steps) and
// anything else that does not parse as a synthesized issue step are
// always kept. Issue-batch only; explicit Pipeline mode returns the
// input unchanged.
func activeChildren(
	w *meithealv1alpha1.Workload, children []meithealv1alpha1.AgenticTask,
) []meithealv1alpha1.AgenticTask {
	if len(w.Spec.Pipeline) > 0 || len(w.Spec.Issues) == 0 {
		return children
	}

	type reviewerRound struct {
		n      int32
		idx, k int
	}
	latest := make(map[int32]int, len(w.Spec.Issues))
	escalated := make(map[int32]bool, len(w.Spec.Issues))
	rerun := map[reviewerRound]bool{}
	for i := range children {
		step := children[i].Labels[labelStep]
		for _, n := range w.Spec.Issues {
			if idx, k, rereview, ok := parseReviewStep(step, n); ok && rereview {
				rerun[reviewerRound{n, idx, k}] = true
			}
			if step == fmt.Sprintf("code-%d-esc", n) {
				escalated[n] = true
				break
			}
			if k, ok := issueStepIteration(step, n); ok {
				if k > latest[n] {
					latest[n] = k
				}
				break
			}
		}
	}

	active := make([]meithealv1alpha1.AgenticTask, 0, len(children))
	for i := range children {
		step := children[i].Labels[labelStep]
		superseded := false
		for _, n := range w.Spec.Issues {
			if k, ok := issueStepIteration(step, n); ok {
				// A later fix iteration, or a successful escalation on this
				// issue, supersedes the base/earlier synthesized attempt.
				// issueStepIteration never matches the -esc steps, so the
				// escalation attempt is never dropped by its own rule.
				superseded = k < latest[n] || escalated[n]
				if idx, _, rereview, ok := parseReviewStep(step, n); ok && !rereview {
					superseded = superseded || rerun[reviewerRound{n, idx, k}]
				}
				break
			}
		}
		if !superseded {
			active = append(active, children[i])
		}
	}
	return active
}

// countReviewIterations sums the latest fix-iteration index per issue
// over the given step names, for status.reviewIterations. Recomputed
// from observed state (not incremented) so reconcile echoes cannot
// inflate it.
func countReviewIterations(w *meithealv1alpha1.Workload, stepNames map[string]struct{}) int32 {
	var total int32
	for _, n := range w.Spec.Issues {
		best := 0
		for step := range stepNames {
			if k, ok := issueStepIteration(step, n); ok && k > best {
				best = k
			}
		}
		total += int32(best)
	}
	return total
}

// recordSuppressedDemotions reports the inert demotions that kept a fix
// iteration from opening (#1636), naming each review task and the rail's
// demotionReason. Same contract as the MaxTasks cap twenty lines below: no
// silent skip. The Workload still rolls to Failed on the demoted NO-GO, so
// the condition is what turns "Failed with no explanation" into "Failed
// because the issueAsk rail could not verify the reviewer read the issue".
func (r *WorkloadReconciler) recordSuppressedDemotions(
	ctx context.Context, w *meithealv1alpha1.Workload, suppressed []suppressedDemotion,
) error {
	return r.recordNoIteration(ctx, w, conditionTypeReviewIterationSuppressed, "InertDemotion",
		"the issueAsk rail rewrote the reviewer's GO and the review carries no findings, so the iteration would hand the coder an approval it cannot act on",
		suppressed)
}

// recordUntrustedReviews reports the re-reviews whose NO-GO was untrusted
// again, so the issue ended without a fix iteration (#41). Like the inert
// case, the NO-GO still rolls the Workload to Failed; when escalation
// reviewers are configured, emitEscalations also fires them on it, so the
// branch is judged by a bigger model the way a demoted GO is.
func (r *WorkloadReconciler) recordUntrustedReviews(
	ctx context.Context, w *meithealv1alpha1.Workload, untrusted []suppressedDemotion,
) error {
	why := "the review was re-run once on the same branch and the re-review's NO-GO is untrusted too, so the iteration would hand the coder findings the harness does not believe; the issue ends incomplete"
	if len(w.Spec.EscalationReviewerAgentRefs) > 0 {
		why += " and goes to the escalation reviewers"
	}
	return r.recordNoIteration(ctx, w, conditionTypeReviewUntrusted, "UntrustedAfterRereview", why, untrusted)
}

// recordNoIteration sets condType to say that the listed NO-GO reviews
// opened no fix iteration and why, naming each review task with its rail's
// reason, and fires the paired Warning event.
//
// The condition carries the record because it survives; the event surfaces
// it in `kubectl describe` while the operator is looking. Both are skipped
// when the stored condition already says exactly this, so a steady-state
// reconcile neither churns LastTransitionTime nor re-fires the event.
func (r *WorkloadReconciler) recordNoIteration(
	ctx context.Context, w *meithealv1alpha1.Workload,
	condType, reason, why string, reviews []suppressedDemotion,
) error {
	if len(reviews) == 0 {
		return nil
	}

	parts := make([]string, 0, len(reviews))
	for _, s := range reviews {
		parts = append(parts, joinStepReason(s))
	}
	msg := fmt.Sprintf("no fix iteration for %d NO-GO review task(s): %s (%s)",
		len(reviews), why, strings.Join(parts, "; "))

	cond := metav1.Condition{
		Type:    condType,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: msg,
	}
	if existing := apimeta.FindStatusCondition(w.Status.Conditions, condType); existing != nil &&
		existing.Status == cond.Status && existing.Reason == cond.Reason && existing.Message == cond.Message {
		return nil
	}
	cond.LastTransitionTime = metav1.Now()

	patch := client.MergeFrom(w.DeepCopy())
	setCondition(&w.Status.Conditions, cond)
	if err := r.Status().Patch(ctx, w, patch); err != nil {
		return fmt.Errorf("patch %s condition: %w", condType, err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(w, nil, corev1.EventTypeWarning, condType, "Reconcile", "%s", msg)
	}
	return nil
}

// emissionConditions renders the conditions one emission pass sets:
// ReviewIterationTriggered for the issues a fix iteration was emitted for,
// and ReviewRerunTriggered (#41) for the issues an untrusted review was
// re-run for. The created / suppressed counts are split between the two by
// step name, so each condition counts its own tasks. planned is every step
// the pass proposed and kept is what survived the sovereignty filter.
func emissionConditions(
	plan iterationPlan, planned, kept []meithealv1alpha1.PipelineStep, created []corev1.ObjectReference,
) []metav1.Condition {
	var plannedRerun, keptRerun, createdRerun int
	for _, s := range planned {
		if strings.HasSuffix(s.Name, rereviewSuffix) {
			plannedRerun++
		}
	}
	for _, s := range kept {
		if strings.HasSuffix(s.Name, rereviewSuffix) {
			keptRerun++
		}
	}
	for _, ref := range created {
		if strings.HasSuffix(ref.Name, rereviewSuffix) {
			createdRerun++
		}
	}
	droppedRerun := plannedRerun - keptRerun
	droppedIter := len(planned) - len(kept) - droppedRerun

	var conds []metav1.Condition
	if len(plan.iterated) > 0 {
		conds = append(conds, metav1.Condition{
			Type:   conditionTypeReviewIterationTriggered,
			Status: metav1.ConditionTrue,
			Reason: "ReviewerNoGo",
			Message: fmt.Sprintf("issues %s re-dispatched to the coder after reviewer NO-GO (%d task(s) created, %d suppressed)",
				joinInt32(plan.iterated), len(created)-createdRerun, droppedIter),
		})
	}
	if len(plan.rereviewed) > 0 {
		parts := make([]string, 0, len(plan.rerunOf))
		for _, s := range plan.rerunOf {
			parts = append(parts, joinStepReason(s))
		}
		conds = append(conds, metav1.Condition{
			Type:   conditionTypeReviewRerunTriggered,
			Status: metav1.ConditionTrue,
			Reason: "UntrustedReview",
			Message: fmt.Sprintf("issues %s re-reviewed once on the same branch instead of opening a fix iteration, because the reviewer's NO-GO is untrusted (%d task(s) created, %d suppressed): %s",
				joinInt32(plan.rereviewed), createdRerun, droppedRerun, strings.Join(parts, "; ")),
		})
	}
	return conds
}

// joinStepReason renders a review for a condition message: "<step>: <reason>",
// or the bare step when the rail recorded no reason.
func joinStepReason(s suppressedDemotion) string {
	if s.reason == "" {
		return s.step
	}
	return s.step + ": " + s.reason
}

// conditionsCurrent reports whether every condition in want is already
// stored with the same status, reason and message.
func conditionsCurrent(stored []metav1.Condition, want []metav1.Condition) bool {
	for _, c := range want {
		existing := apimeta.FindStatusCondition(stored, c.Type)
		if existing == nil || existing.Status != c.Status ||
			existing.Reason != c.Reason || existing.Message != c.Message {
			return false
		}
	}
	return true
}

// emitReviewIterations is the fix-iteration emission hook (#946):
// called from Reconcile's children-exist branch after the advisory
// wiring and before escalation + rollup. Synthesizes the due
// code/verify/review -r<k> steps and the due re-reviews (#41), applies
// MaxTasks accounting and the sovereignty gates, creates the tasks, and
// patches status (Tasks list + reviewIterations + ReviewIterationTriggered /
// ReviewRerunTriggered + CloudReviewersSuppressed / Truncated as
// applicable). Returns the
// refreshed children slice so escalation and rollup see the new tasks
// as in-flight; on a no-op it returns the input slice.
func (r *WorkloadReconciler) emitReviewIterations(
	ctx context.Context, w *meithealv1alpha1.Workload, children []meithealv1alpha1.AgenticTask,
) ([]meithealv1alpha1.AgenticTask, error) {
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", client.ObjectKeyFromObject(w))

	if len(w.Spec.Pipeline) > 0 {
		// Explicit Pipeline mode: user-authored step names could
		// false-match the synthesized code/verify/review-<n> naming the
		// iteration walker scans. Iteration is an issue-batch feature.
		return children, nil
	}

	plan := reviewIterationSteps(w, children)
	steps := plan.steps

	// Before the no-op short-circuit: a round that suppressed an inert
	// demotion, or ended on an untrusted re-review, emits NO steps at all,
	// and that is exactly the case an operator needs told about.
	if err := r.recordSuppressedDemotions(ctx, w, plan.suppressed); err != nil {
		return children, err
	}
	if err := r.recordUntrustedReviews(ctx, w, plan.untrusted); err != nil {
		return children, err
	}

	if len(steps) == 0 {
		return children, nil
	}

	patch := client.MergeFrom(w.DeepCopy())
	now := metav1.Now()

	if w.Spec.MaxTasks > 0 && len(children)+len(steps) > int(w.Spec.MaxTasks) {
		// No silent cap: report why the fix iteration did not run. The
		// terminal NO-GO then rolls the Workload to Failed as before.
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeTruncated,
			Status:             metav1.ConditionTrue,
			Reason:             "MaxTasksIterationCap",
			Message:            fmt.Sprintf("MaxTasks=%d leaves no room for %d fix-iteration task(s)", w.Spec.MaxTasks, len(steps)),
			LastTransitionTime: now,
		})
		if err := r.Status().Patch(ctx, w, patch); err != nil {
			return children, fmt.Errorf("patch iteration truncation condition: %w", err)
		}
		return children, nil
	}

	planned := steps
	steps, suppressed, err := r.filterCloudProviders(ctx, w, steps)
	if err != nil {
		return children, fmt.Errorf("filter iteration providers: %w", err)
	}

	// Revision-vs-profile pairing (#951): a fix-iteration coder task
	// amends its restored prior attempt, and the issue-fix Agent's
	// forcing profile is tuned for building a fix from scratch — the
	// documented #951 failure mode is a revision task collapsing under
	// it. Warn (once per emission pass) when the fallback is in play so
	// the operator knows to set spec.revisionCoderAgentRef.
	if r.Recorder != nil && w.Spec.RevisionCoderAgentRef == nil {
		for _, s := range steps {
			if s.Kind != meithealv1alpha1.AgenticTaskKindIssueFix {
				continue
			}
			r.Recorder.Eventf(w, nil, corev1.EventTypeWarning, "RevisionUnderIssueFixProfile", "Reconcile",
				"fix-iteration coder step %s falls back to the issue-fix Agent %q; a revision task amends its restored prior attempt and can collapse under the issue-fix forcing profile (#951) — set spec.revisionCoderAgentRef to pair iterations with a revision-tuned profile",
				s.Name, w.Spec.CoderAgentRef.Name)
			break
		}
	}

	created, createErr := r.renderAndCreate(ctx, w, steps)
	if createErr != nil {
		log.Error(createErr, "creating fix-iteration AgenticTasks failed mid-way", "createdSoFar", len(created))
	}

	conds := emissionConditions(plan, planned, steps, created)

	// Steady-state short-circuit: when nothing was created (e.g. the
	// only missing steps are cloud-suppressed reviewers) and the
	// conditions already say exactly this, re-patching every rollup
	// would only churn LastTransitionTime.
	if len(created) == 0 && createErr == nil && conditionsCurrent(w.Status.Conditions, conds) {
		return children, nil
	}

	stepNames := make(map[string]struct{}, len(children)+len(created))
	for i := range children {
		if step := children[i].Labels[labelStep]; step != "" {
			stepNames[step] = struct{}{}
		}
	}
	for _, ref := range created {
		stepNames[strings.TrimPrefix(ref.Name, w.Name+"-")] = struct{}{}
	}

	w.Status.Tasks = appendNewTaskRefs(w.Status.Tasks, created)
	w.Status.ReviewIterations = countReviewIterations(w, stepNames)
	for _, cond := range conds {
		cond.LastTransitionTime = now
		setCondition(&w.Status.Conditions, cond)
	}
	if len(suppressed) > 0 {
		setCondition(&w.Status.Conditions, metav1.Condition{
			Type:               conditionTypeCloudReviewersSuppressed,
			Status:             metav1.ConditionTrue,
			Reason:             "SovereigntyGate",
			Message:            fmt.Sprintf("skipped %d cloud-provider Agent(s): %s", len(suppressed), strings.Join(suppressed, "; ")),
			LastTransitionTime: now,
		})
	}
	if err := r.Status().Patch(ctx, w, patch); err != nil {
		return children, fmt.Errorf("patch workload status after iteration emission: %w", err)
	}
	if createErr != nil {
		return children, createErr
	}

	// Escalation + rollup must see the new tasks as in-flight in this
	// same pass. A cache-backed List here could miss tasks created
	// microseconds ago (informer lag), so synthesize labeled
	// placeholders instead: a zero-value phase counts as in-flight, and
	// the step label lets activeChildren + the escalation trigger
	// recognize the new round immediately (without it, the superseded
	// round's terminal NO-GO would fire escalation one pass early).
	existing := make(map[string]struct{}, len(children))
	for i := range children {
		existing[children[i].Name] = struct{}{}
	}
	for _, ref := range created {
		if _, ok := existing[ref.Name]; ok {
			continue
		}
		children = append(children, meithealv1alpha1.AgenticTask{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ref.Name,
				Namespace: ref.Namespace,
				Labels: map[string]string{
					labelWorkload: w.Name,
					labelStep:     strings.TrimPrefix(ref.Name, w.Name+"-"),
				},
			},
		})
	}
	return children, nil
}
