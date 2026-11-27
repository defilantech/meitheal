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

package agent

import (
	"fmt"
	"path"
	"strings"

	"github.com/go-logr/logr"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent/reviewer"
)

// enforceReviewerOutOfDiffFindings marks a NO-GO untrusted when every one of
// its blocking findings cites a file the branch diff does not touch (#41).
// The verdict is returned unchanged: this rail is a statement about whether
// the rejection can be believed, and the controller answers it by re-running
// the review once instead of opening a fix iteration the coder cannot act on.
//
// The grounded-finding rail already demotes a NO-GO none of whose blocking
// findings cites a changed line, so for most reviews this rail never sees an
// all-out-of-diff NO-GO. It covers what that rail leaves standing: a REJECT
// (exempt from grounded demotion) and a fleet running with
// MEITHEAL_GROUNDED_FINDINGS=0. In #41 the reviewer's blockers named files
// absent from the branch entirely; the coder answered that it could not find
// them, and the Workload failed on a branch a correct review would approve.
//
// A review stays trusted when any blocking finding:
//   - cites a file in the diff, directly or as a basename that resolves to
//     exactly one diff file (resolveAgainstDiff);
//   - cites no file at all, since nothing proves it is out of scope;
//   - cites a file the issue itself names (scopeRefs, recorded by the
//     scope-overlap rail that runs first). A wrong-scope rejection points at
//     the file the issue asks about, which the diff by definition misses.
//
// A missing or empty diff is recorded as a skipped rail: with no ground
// truth there is nothing to compare against, and the verdict stands as the
// reviewer returned it.
func enforceReviewerOutOfDiffFindings(
	log logr.Logger,
	extra map[string]any,
	verdict meithealv1alpha1.AgenticTaskVerdict,
	reviewDiff []string,
	reviewDiffErr error,
) meithealv1alpha1.AgenticTaskVerdict {
	if verdict != meithealv1alpha1.AgenticTaskVerdictNoGo || extra == nil {
		return verdict
	}
	if reviewDiffErr != nil {
		recordRailSkipped(extra, railOutOfDiffFindings, skipReasonNoDiff)
		return verdict
	}
	if len(reviewDiff) == 0 {
		recordRailSkipped(extra, railOutOfDiffFindings, skipReasonNoDiffFiles)
		return verdict
	}

	findings, _ := reviewer.ParseFindings(extra)
	scopeRefs, _ := extra["scopeRefs"].([]string)
	var outside []string
	for _, f := range findings {
		if f.Severity != reviewer.SeverityBlocker && f.Severity != reviewer.SeverityMajor {
			continue
		}
		file := normalizeFilePath(f.File)
		if file == "" || inDiff(file, reviewDiff) || resolveAgainstDiff(file, reviewDiff) != "" ||
			namedByIssue(file, scopeRefs) {
			return verdict
		}
		outside = append(outside, file)
	}
	if len(outside) == 0 {
		return verdict
	}

	// The established demotion markers (#1636): the flag travels with the
	// rail name, and verdictClaimed is first-writer-wins (#1678). Here the
	// archived claim is the reviewer's own NO-GO, which is what tells the
	// controller this NO-GO was asserted by the reviewer and only distrusted
	// by the harness.
	extra["verdictDemoted"] = true
	extra["verdictDemotedBy"] = railOutOfDiffFindings
	if _, ok := extra["verdictClaimed"]; !ok {
		extra["verdictClaimed"] = string(verdict)
	}
	extra["outOfDiffFindingFiles"] = outside
	extra["demotionReason"] = fmt.Sprintf(
		"every blocking finding cites a file the branch diff does not touch (%s); "+
			"review verdict is untrusted", strings.Join(outside, ", "))
	log.Info("reviewer integrity: every blocking finding cites a file outside the diff; "+
		"keeping NO-GO but marking untrusted",
		"files", outside, "diffFiles", reviewDiff)
	return verdict
}

// namedByIssue reports whether file is one the issue body names, matched the
// way the scope-overlap rail matches its refs: by full path or by basename.
func namedByIssue(file string, scopeRefs []string) bool {
	for _, ref := range scopeRefs {
		if file == ref || path.Base(file) == path.Base(ref) {
			return true
		}
	}
	return false
}
