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
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// summaryEvidenceCap bounds each task summary a failure park keeps as
// evidence. A summary is model output and can run long; the decision file
// needs enough to choose an answer, not the whole review.
const summaryEvidenceCap = 2 * 1024

// defaultMaxReviewIterations is the WorkloadReconciler's per-issue bound on
// fix iterations when spec.maxReviewIterations is unset
// (internal/controller/workload_iteration.go). The two must agree, or a run
// that hit the cap would be reported as one that stopped short of it.
const defaultMaxReviewIterations = 1

// maxReviewIterations resolves a Workload's fix-iteration bound the way the
// reconciler does: nil is the default, an explicit value (0 included) wins.
func maxReviewIterations(w *meithealv1alpha1.Workload) int {
	if w == nil || w.Spec.MaxReviewIterations == nil {
		return defaultMaxReviewIterations
	}
	return int(*w.Spec.MaxReviewIterations)
}

// The synthesized issue-batch step names (internal/controller/
// workload_iteration.go and workload_coder_escalation.go): code-<n>,
// verify-<n>, review-<n>-<i>, each with "-r<k>" for fix iteration k, and the
// coder-escalation attempt's code-<n>-esc, verify-<n>-esc, review-<n>-esc-<i>.
// A run Workload carries one issue, so <n> is not compared.
var (
	coderStepRE  = regexp.MustCompile(`^code-\d+(?:-r([1-9]\d*))?$`)
	verifyStepRE = regexp.MustCompile(`^verify-\d+(?:-r([1-9]\d*))?$`)
	reviewStepRE = regexp.MustCompile(`^review-\d+-\d+(?:-r([1-9]\d*))?$`)
	escStepRE    = regexp.MustCompile(`^(?:(code)-\d+-esc|(verify)-\d+-esc|(review)-\d+-esc-\d+)$`)
)

type stepRole int

const (
	roleCoder stepRole = iota
	roleVerify
	roleReview
)

// round is one attempt's steps: the coder, the gate that checked it, and the
// reviewers that judged it.
type round struct {
	// esc marks the coder-escalation attempt, which the reconciler never
	// iterates: a NO-GO there ends the run.
	esc     bool
	coder   *meithealv1alpha1.AgenticTask
	verify  *meithealv1alpha1.AgenticTask
	reviews []*meithealv1alpha1.AgenticTask
}

// stepOf names a child by its step label, or by its object name less the
// Workload prefix when the label is missing.
func stepOf(t *meithealv1alpha1.AgenticTask) string {
	if s := t.Labels["meitheal.dev/step"]; s != "" {
		return s
	}
	return strings.TrimPrefix(t.Name, t.Labels["meitheal.dev/workload"]+"-")
}

// parseStep classifies a step name: its role, its fix iteration, and whether
// it belongs to the coder-escalation attempt.
func parseStep(step string) (role stepRole, iter int, esc, ok bool) {
	if m := escStepRE.FindStringSubmatch(step); m != nil {
		switch {
		case m[1] != "":
			return roleCoder, 0, true, true
		case m[2] != "":
			return roleVerify, 0, true, true
		default:
			return roleReview, 0, true, true
		}
	}
	for _, c := range []struct {
		re   *regexp.Regexp
		role stepRole
	}{{coderStepRE, roleCoder}, {verifyStepRE, roleVerify}, {reviewStepRE, roleReview}} {
		m := c.re.FindStringSubmatch(step)
		if m == nil {
			continue
		}
		if m[1] == "" {
			return c.role, 0, false, true
		}
		k, err := strconv.Atoi(m[1])
		return c.role, k, false, err == nil
	}
	return 0, 0, false, false
}

// roundsOf groups the children by fix iteration, ordered oldest first. Once
// a coder-escalation attempt exists it supersedes the base attempt (as the
// reconciler's rollup does) and is the only round returned. Children that are
// not synthesized issue steps are ignored.
func roundsOf(tasks []meithealv1alpha1.AgenticTask) []*round {
	base, esc := map[int]*round{}, &round{esc: true}
	hasEsc := false
	for i := range tasks {
		t := &tasks[i]
		role, iter, isEsc, ok := parseStep(stepOf(t))
		if !ok {
			continue
		}
		r := esc
		if isEsc {
			hasEsc = true
		} else {
			if base[iter] == nil {
				base[iter] = &round{}
			}
			r = base[iter]
		}
		switch role {
		case roleCoder:
			r.coder = t
		case roleVerify:
			r.verify = t
		case roleReview:
			r.reviews = append(r.reviews, t)
		}
	}
	var out []*round
	if hasEsc {
		out = []*round{esc}
	} else {
		iters := make([]int, 0, len(base))
		for k := range base {
			iters = append(iters, k)
		}
		sort.Ints(iters)
		for _, k := range iters {
			out = append(out, base[k])
		}
	}
	for _, r := range out {
		sort.Slice(r.reviews, func(i, j int) bool { return r.reviews[i].Name < r.reviews[j].Name })
	}
	return out
}

// missedTarget reports a terminal task that did not succeed on target.
func missedTarget(t *meithealv1alpha1.AgenticTask) bool {
	if t == nil {
		return false
	}
	switch t.Status.Phase {
	case meithealv1alpha1.AgenticTaskPhaseSucceeded, meithealv1alpha1.AgenticTaskPhaseFailed:
		return !t.SucceededOnTarget()
	}
	return false
}

// taskSummary is what a task said about its outcome: its result's summary,
// or its Completed condition's message when it has no result (an executor
// error, a cascade-fail). Capped, since it is model or log output.
func taskSummary(t *meithealv1alpha1.AgenticTask) string {
	if t == nil {
		return ""
	}
	if t.Status.Result != nil && len(t.Status.Result.Raw) > 0 {
		var r struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(t.Status.Result.Raw, &r) == nil && strings.TrimSpace(r.Summary) != "" {
			return capString(strings.TrimSpace(r.Summary), summaryEvidenceCap)
		}
	}
	if c := apimeta.FindStatusCondition(t.Status.Conditions, "Completed"); c != nil {
		return capString(strings.TrimSpace(c.Message), summaryEvidenceCap)
	}
	return ""
}

func hasVerdict(reviews []*meithealv1alpha1.AgenticTask, v meithealv1alpha1.AgenticTaskVerdict) bool {
	for _, t := range reviews {
		if t.Status.Verdict == v {
			return true
		}
	}
	return false
}

// latestReviewSummary is the newest review verdict's summary: a NO-GO
// reviewer's ahead of a GO reviewer's in the same round. A review with no
// verdict (crashed, cascade-failed) said nothing about the change and is
// skipped.
func latestReviewSummary(rounds []*round) string {
	for i := len(rounds) - 1; i >= 0; i-- {
		for _, v := range []meithealv1alpha1.AgenticTaskVerdict{
			meithealv1alpha1.AgenticTaskVerdictNoGo, meithealv1alpha1.AgenticTaskVerdictGo,
		} {
			for _, t := range rounds[i].reviews {
				if t.Status.Verdict != v {
					continue
				}
				if s := taskSummary(t); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// latestCoderSummary is the newest coder summary there is.
func latestCoderSummary(rounds []*round) string {
	for i := len(rounds) - 1; i >= 0; i-- {
		if s := taskSummary(rounds[i].coder); s != "" {
			return s
		}
	}
	return ""
}

func roundsWord(n int) string {
	if n == 1 {
		return "1 round"
	}
	return fmt.Sprintf("%d rounds", n)
}

// explainFailure derives a failed Workload's park reason from its children's
// terminal outcomes, and the evidence behind it: the latest reviewer summary
// ("review"), the coder's last summary ("coder") and, for a gate failure, the
// gate's ("gate"). maxIter is the Workload's fix-iteration bound.
//
// Rounds are read off the step names, not creation times. The newest round
// decides: a coder that missed its target there could not answer the
// previous round's NO-GO (or, in the first round, failed before any review
// verdict); a gate that failed there is the gate's failure; a NO-GO there is
// the review's, at the iteration cap or short of it.
//
// The reason is public (the park comment carries it), so it is built from
// verdicts and round numbers only, never a task's name or text. The evidence
// goes to the decision file and the private notify hook only.
func explainFailure(tasks []meithealv1alpha1.AgenticTask, maxIter int) (string, map[string]string) {
	rounds := roundsOf(tasks)
	if len(rounds) == 0 {
		return RunFailedReason, nil
	}
	ev := map[string]string{}
	if s := latestReviewSummary(rounds); s != "" {
		ev["review"] = s
	}
	if s := latestCoderSummary(rounds); s != "" {
		ev["coder"] = s
	}
	r := len(rounds) - 1
	last := rounds[r]
	reason := RunFailedReason
	switch {
	case missedTarget(last.coder):
		// Fix iteration r answers review round r; there is none before
		// the first coder.
		switch {
		case r == 0:
		case last.coder.Status.Verdict == meithealv1alpha1.AgenticTaskVerdictNoGo:
			reason = fmt.Sprintf("review returned NO-GO in round %d and the coder could not address it", r)
		default:
			reason = fmt.Sprintf("review returned NO-GO in round %d and the coder's fix attempt failed", r)
		}
	case missedTarget(last.verify):
		which := "on the coder's first attempt"
		switch {
		case last.esc:
			which = "on the escalated coder's attempt"
		case r > 0:
			which = fmt.Sprintf("on the coder's fix for review round %d", r)
		}
		switch v := last.verify.Status.Verdict; v {
		case meithealv1alpha1.AgenticTaskVerdictGateFail, meithealv1alpha1.AgenticTaskVerdictGateError:
			reason = fmt.Sprintf("the gate returned %s %s", v, which)
		default:
			reason = "the gate did not return a verdict " + which
		}
		if s := taskSummary(last.verify); s != "" {
			ev["gate"] = s
		}
	case hasVerdict(last.reviews, meithealv1alpha1.AgenticTaskVerdictNoGo):
		switch {
		case last.esc:
			reason = "review returned NO-GO on the escalated coder's attempt"
		case r >= maxIter:
			reason = fmt.Sprintf("review returned NO-GO after %s (iteration cap)", roundsWord(r+1))
		default:
			reason = fmt.Sprintf("review returned NO-GO in round %d and no fix iteration followed", r+1)
		}
	case r > 0 && hasMissedReview(last.reviews):
		reason = fmt.Sprintf("review returned NO-GO in round %d and the next review did not return a verdict", r)
	}
	if len(ev) == 0 {
		ev = nil
	}
	return reason, ev
}

func hasMissedReview(reviews []*meithealv1alpha1.AgenticTask) bool {
	for _, t := range reviews {
		if missedTarget(t) {
			return true
		}
	}
	return false
}
