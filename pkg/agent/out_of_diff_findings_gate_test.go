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
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent/reviewer"
)

// outOfDiffExtra builds a reviewer submit_result extra carrying the given
// findings, each written as {severity, file, line}.
func outOfDiffExtra(findings ...map[string]any) map[string]any {
	list := make([]any, 0, len(findings))
	for _, f := range findings {
		f["area"] = "scope"
		f["message"] = "m"
		list = append(list, f)
	}
	return map[string]any{"findings": list}
}

func oodFinding(severity, file string, line int) map[string]any {
	return map[string]any{"severity": severity, "file": file, "line": line}
}

// TestOutOfDiffFindings covers the rail that marks a NO-GO untrusted when
// every blocking finding cites a file the branch diff does not touch (#41).
// The rail never changes the verdict; it only stamps the demotion markers the
// controller reads to re-run the review instead of opening a fix iteration.
func TestOutOfDiffFindings(t *testing.T) {
	noGo := meithealv1alpha1.AgenticTaskVerdictNoGo
	goVerdict := meithealv1alpha1.AgenticTaskVerdictGo
	diff := []string{"docs/guide.md"}

	cases := []struct {
		name      string
		extra     map[string]any
		verdict   meithealv1alpha1.AgenticTaskVerdict
		diff      []string
		diffErr   error
		wantMark  bool
		wantFiles []string
		wantSkip  string
	}{
		{
			// The #41 shape: the diff adds one doc page and every blocker
			// cites source files the branch never touched.
			name: "every blocker cites a file outside the diff",
			extra: outOfDiffExtra(
				oodFinding("blocker", "internal/controller/foo.go", 12),
				oodFinding("major", "./pkg/agent/bar.go", 3)),
			verdict:   noGo,
			diff:      diff,
			wantMark:  true,
			wantFiles: []string{"internal/controller/foo.go", "pkg/agent/bar.go"},
		},
		{
			name: "one blocker cites the diff: rejection stands untouched",
			extra: outOfDiffExtra(
				oodFinding("blocker", "internal/controller/foo.go", 12),
				oodFinding("blocker", "docs/guide.md", 4)),
			verdict: noGo,
			diff:    diff,
		},
		{
			// resolveAgainstDiff: a bare basename that uniquely names a diff
			// file is the reviewer citing the diff, not a fabrication.
			name:    "a basename resolving to a diff file counts as in the diff",
			extra:   outOfDiffExtra(oodFinding("blocker", "guide.md", 4)),
			verdict: noGo,
			diff:    diff,
		},
		{
			// A blocking finding that cites no file is not provably outside
			// the diff, so the rail cannot call the review untrusted.
			name: "a blocker with no file keeps the review trusted",
			extra: outOfDiffExtra(
				oodFinding("blocker", "internal/controller/foo.go", 12),
				oodFinding("blocker", "", 0)),
			verdict: noGo,
			diff:    diff,
		},
		{
			// Minor findings are advisory, exactly as in the grounded-finding
			// rail; with no blocking finding there is nothing to judge.
			name:    "minor-only findings are not judged",
			extra:   outOfDiffExtra(oodFinding("minor", "internal/controller/foo.go", 12)),
			verdict: noGo,
			diff:    diff,
		},
		{
			// A wrong-scope rejection names the file the issue asks about,
			// which the diff by definition does not touch. scopeRefs is what
			// the scope-overlap rail extracted from the issue body.
			name: "a blocker citing a file the issue names keeps the review trusted",
			extra: func() map[string]any {
				e := outOfDiffExtra(oodFinding("blocker", "internal/controller/foo.go", 12))
				e["scopeRefs"] = []string{"internal/controller/foo.go"}
				return e
			}(),
			verdict: noGo,
			diff:    diff,
		},
		{
			name:    "GO is not this rail's business",
			extra:   outOfDiffExtra(oodFinding("blocker", "internal/controller/foo.go", 12)),
			verdict: goVerdict,
			diff:    diff,
		},
		{
			name:     "diff unavailable: recorded as skipped, not marked",
			extra:    outOfDiffExtra(oodFinding("blocker", "internal/controller/foo.go", 12)),
			verdict:  noGo,
			diffErr:  errors.New("git: bad revision"),
			wantSkip: railOutOfDiffFindings + ": " + skipReasonNoDiff,
		},
		{
			name:     "empty diff: recorded as skipped, not marked",
			extra:    outOfDiffExtra(oodFinding("blocker", "internal/controller/foo.go", 12)),
			verdict:  noGo,
			wantSkip: railOutOfDiffFindings + ": " + skipReasonNoDiffFiles,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := enforceReviewerOutOfDiffFindings(logr.Discard(), tc.extra, tc.verdict, tc.diff, tc.diffErr)
			if got != tc.verdict {
				t.Fatalf("verdict = %s, want %s unchanged: the rail marks, it never rewrites", got, tc.verdict)
			}

			by, _ := tc.extra["verdictDemotedBy"].(string)
			if !tc.wantMark {
				if by != "" || tc.extra["verdictDemoted"] != nil {
					t.Fatalf("review must not be marked untrusted, got verdictDemotedBy=%q extra=%v", by, tc.extra)
				}
			} else {
				if by != reviewer.RailOutOfDiffFindings {
					t.Fatalf("verdictDemotedBy = %q, want %q", by, reviewer.RailOutOfDiffFindings)
				}
				if tc.extra["verdictDemoted"] != true {
					t.Fatalf("verdictDemoted = %v, want true (the flag travels with the rail name)", tc.extra["verdictDemoted"])
				}
				if claimed := tc.extra["verdictClaimed"]; claimed != string(noGo) {
					t.Fatalf("verdictClaimed = %v, want the reviewer's own NO-GO archived", claimed)
				}
				reason, _ := tc.extra["demotionReason"].(string)
				if !strings.Contains(reason, "untrusted") {
					t.Fatalf("demotionReason must say the review is untrusted, got %q", reason)
				}
				files, _ := tc.extra["outOfDiffFindingFiles"].([]string)
				if strings.Join(files, ",") != strings.Join(tc.wantFiles, ",") {
					t.Fatalf("outOfDiffFindingFiles = %v, want %v", files, tc.wantFiles)
				}
				for _, f := range tc.wantFiles {
					if !strings.Contains(reason, f) {
						t.Fatalf("demotionReason must name %s, got %q", f, reason)
					}
				}
			}

			skipped, _ := tc.extra[railsSkippedKey].([]string)
			if tc.wantSkip == "" {
				if len(skipped) != 0 {
					t.Fatalf("railsSkipped = %v, want none", skipped)
				}
			} else if len(skipped) != 1 || skipped[0] != tc.wantSkip {
				t.Fatalf("railsSkipped = %v, want [%s]", skipped, tc.wantSkip)
			}
		})
	}
}

// TestOutOfDiffFindings_KeepsEarlierClaim pins first-writer-wins on
// verdictClaimed, the contract every demoting rail shares (#1678).
func TestOutOfDiffFindings_KeepsEarlierClaim(t *testing.T) {
	extra := outOfDiffExtra(oodFinding("blocker", "internal/controller/foo.go", 12))
	extra["verdictClaimed"] = "GO"
	enforceReviewerOutOfDiffFindings(logr.Discard(), extra, meithealv1alpha1.AgenticTaskVerdictNoGo,
		[]string{"docs/guide.md"}, nil)
	if extra["verdictClaimed"] != "GO" {
		t.Fatalf("verdictClaimed = %v, want the earlier rail's archive kept", extra["verdictClaimed"])
	}
}

func TestOutOfDiffFindings_NilExtraDoesNotPanic(t *testing.T) {
	got := enforceReviewerOutOfDiffFindings(logr.Discard(), nil, meithealv1alpha1.AgenticTaskVerdictNoGo,
		[]string{"docs/guide.md"}, nil)
	if got != meithealv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("verdict = %s, want NO-GO", got)
	}
}
