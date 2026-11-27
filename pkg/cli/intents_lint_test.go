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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// lintFixtureFiles is the fixture repository the lint tests check against.
func lintFixtureFiles() map[string]string {
	return map[string]string{
		"pkg/cli/intents.go":       "package cli\n",
		"pkg/cli/intents_test.go":  "package cli\n",
		"docs/run-loop.md":         "# Run loop\n",
		"docs/images/flow.png":     "png",
		"cmd/meitheal/main.go":     "package main\n",
		".github/workflows/ci.yml": "on: push\n",
		"README.md":                "# widgets\n",
		"Makefile":                 "all:\n",
	}
}

func lintFixture(t *testing.T) repoPaths {
	t.Helper()
	listing, err := repoFiles(gitRepo(t, lintFixtureFiles()))
	if err != nil {
		t.Fatal(err)
	}
	return newRepoPaths(listing.Files)
}

func TestLintIntent(t *testing.T) {
	repo := lintFixture(t)
	type failure struct {
		check  string
		tokens []string
	}
	cases := []struct {
		name   string
		files  []string
		intent string
		want   []failure
	}{
		{
			name:  "existing paths in the forms an intent writes them",
			files: []string{"pkg/cli/intents.go"},
			intent: "Edit `pkg/cli/intents.go` (see pkg/cli/intents.go:120 and ./pkg/cli/intents_test.go).\n" +
				"Mirror **docs/run-loop.md**, [the template](docs/run-loop.md#the-template) and docs/images/.\n" +
				"Run `go test ./pkg/cli/...` and `go build ./cmd/meitheal`. Call pkg/cli.ParsePlanVerdict.\n" +
				"The workflow is .github/workflows/ci.yml.",
		},
		{
			name:  "URLs, import paths, globs, flags and absolute paths are not checked",
			files: []string{"pkg/cli/intents.go"},
			intent: "Edit pkg/cli/intents.go. See https://example.com/widgets/blob/main/docs/missing.md and\n" +
				"import github.com/example/widgets/pkg/missing. Match docs/*.md and pkg/**/gone.go.\n" +
				"Pass --rules-file=docs/run-loop.md -o /tmp/docs/out.md, $HOME/docs/x.md, ~/docs/y.md.\n" +
				"Write the binary to bin/meitheal and the intents to .meitheal/intents/52.intent.md.",
		},
		{
			name:   "a root-level name without a slash is not checked",
			files:  []string{"pkg/cli/intents.go"},
			intent: "Edit pkg/cli/intents.go and add a line to CHANGES.md and intents.go.",
		},
		{
			name:   "a file to create is in the header, and so is the directory it creates",
			files:  []string{"docs/guides/intent-lint.md", "pkg/cli/intents.go"},
			intent: "Create docs/guides/intent-lint.md under docs/guides/, linked from pkg/cli/intents.go.",
		},
		{
			name:   "a header file the intent names by its base name is mentioned",
			files:  []string{"pkg/cli/intents.go"},
			intent: "Open `intents.go` in the cli package and change planOne.",
		},
		{
			name:  "a path that does not exist",
			files: []string{"pkg/cli/intents.go"},
			intent: "Edit pkg/cli/intents.go. Use only the sources in **docs/planner-notes.md** and pkg/cli/intent.go,\n" +
				"and pass --rules-file=docs/rules.md.",
			want: []failure{{lintUnknownPaths,
				[]string{"docs/planner-notes.md", "pkg/cli/intent.go", "docs/rules.md"}}},
		},
		{
			name:   "a file the intent says to create but the header leaves out",
			files:  []string{"pkg/cli/intents.go"},
			intent: "Create `docs/intent-lint.md` and link it from pkg/cli/intents.go.",
			want:   []failure{{lintUnknownPaths, []string{"docs/intent-lint.md"}}},
		},
		{
			name:   "an existing header file the intent never mentions",
			files:  []string{"pkg/cli/intents.go", "pkg/cli/intents_test.go"},
			intent: "Edit pkg/cli/intents.go.",
			want:   []failure{{lintUnmentionedFile, []string{"pkg/cli/intents_test.go"}}},
		},
		{
			name:   "a new header file the intent never mentions is not this check's",
			files:  []string{"pkg/cli/intents.go", "pkg/cli/intents_lint.go"},
			intent: "Edit pkg/cli/intents.go.",
		},
		{
			name:   "both checks fail",
			files:  []string{"README.md"},
			intent: "Rewrite docs/gone.md.",
			want: []failure{
				{lintUnknownPaths, []string{"docs/gone.md"}},
				{lintUnmentionedFile, []string{"README.md"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintIntent(PlanVerdict{Kind: VerdictPlanned, Files: tc.files, Intent: tc.intent}, repo)
			var gotF []failure
			for _, f := range got.Failures {
				gotF = append(gotF, failure{f.Check, f.Tokens})
			}
			if !reflect.DeepEqual(gotF, tc.want) {
				t.Fatalf("failures = %+v, want %+v\nreport:\n%s", gotF, tc.want, got.report())
			}
			if got.ok() != (len(tc.want) == 0) {
				t.Fatalf("ok() = %v with failures %+v", got.ok(), gotF)
			}
		})
	}
}

// The park reason names the failed checks only: it is posted on the public
// issue, so no token from the model's intent may reach it.
func TestLintIntent_ReasonIsFixedAndEvidenceNamesLines(t *testing.T) {
	repo := lintFixture(t)
	intent := "Step one: edit docs/secret-host.example.md.\nStep two: nothing.\n" +
		"Step three: docs/secret-host.example.md again."
	got := lintIntent(PlanVerdict{Files: []string{"README.md"}, Intent: intent}, repo)
	if want := "intent failed lint: unknown paths, files the intent never mentions"; got.reason() != want {
		t.Fatalf("reason = %q, want %q", got.reason(), want)
	}
	if strings.Contains(got.reason(), "secret-host") {
		t.Fatal("reason carries model text")
	}
	want := "unknown paths: docs/secret-host.example.md\n" +
		"  Step one: edit docs/secret-host.example.md.\n" +
		"  Step three: docs/secret-host.example.md again.\n" +
		"files the intent never mentions: README.md\n"
	if got.report() != want {
		t.Fatalf("report = %q, want %q", got.report(), want)
	}
	if got.Checked != 1 {
		t.Fatalf("checked = %d, want 1 distinct path", got.Checked)
	}
}

// lintPlannerServer answers the planner call for each issue in answers,
// found by the prompt's issue header.
func lintPlannerServer(t *testing.T, answers map[int32]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
			t.Errorf("decode request: %v", err)
			return
		}
		answer := "VERDICT: needs-decision\nreason: unknown issue"
		for n, a := range answers {
			if strings.Contains(req.Messages[0].Content, fmt.Sprintf("Issue #%d.", n)) {
				answer = a
			}
		}
		choice := map[string]any{"message": map[string]string{"content": answer}, "finish_reason": "stop"}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{choice}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// End to end through 'meitheal intents': an intent naming a file the
// repository does not have is parked as needs-decision, not queued, and
// with --hold the lint result is printed.
func TestIntents_HoldParksAnIntentThatFailsLint(t *testing.T) {
	for _, hold := range []bool{true, false} {
		t.Run(map[bool]string{true: "hold", false: "no hold"}[hold], func(t *testing.T) {
			root := t.TempDir()
			repoDir := gitRepo(t, lintFixtureFiles())
			srv := lintPlannerServer(t, map[int32]string{
				52: "VERDICT: planned\n```yaml\nlane: cpu\nfiles:\n  - pkg/cli/intents.go\n```\n" +
					"Edit pkg/cli/intents.go, mirroring docs/planner-notes.md.\n",
				53: "VERDICT: planned\n```yaml\nlane: cpu\nfiles:\n  - pkg/cli/intents.go\n```\n" +
					"Edit pkg/cli/intents.go, mirroring docs/run-loop.md.\n",
			})
			o := &intentsOptions{
				repo: planRepo, issues: "52,53", repoDir: repoDir, out: filepath.Join(root, "out"),
				decisionsDir: filepath.Join(root, "decisions"), hold: hold, plannerURL: srv.URL,
				plannerKwargs: defaultPlannerKwargs, plannerTimeout: time.Minute, maxTokens: 100,
				bashTimeout: defaultBashTimeoutSeconds, decisionLabel: "decision",
			}
			var out bytes.Buffer
			cfg, call, err := o.plannerSetup(&out)
			if err != nil {
				t.Fatal(err)
			}
			forge := newPlanForge(map[int32]IssueDetail{52: {Title: "t", Body: "b"}, 53: {Title: "t", Body: "b"}})
			if err := planAndReport(context.Background(), &out, o, forge, call, cfg, []int32{52, 53}); err != nil {
				t.Fatalf("intents: %v", err)
			}

			d := readDecision(t, o.decisionsDir, 52, ParkNeedsDecision)
			assertPlannerPark(t, d, 52, ParkNeedsDecision, "intent failed lint: unknown paths")
			if !strings.Contains(d.Evidence["lint"], "docs/planner-notes.md") ||
				!strings.Contains(d.Evidence["intent"], "mirroring docs/planner-notes.md") {
				t.Fatalf("evidence = %v", d.Evidence)
			}
			if _, err := os.Stat(filepath.Join(o.out, "52.intent.md")); !os.IsNotExist(err) {
				t.Fatalf("a parked intent was written: %v", err)
			}
			b, err := os.ReadFile(filepath.Join(o.out, queueFileName))
			if err != nil {
				t.Fatal(err)
			}
			q, err := ParseQueue(b)
			if err != nil {
				t.Fatal(err)
			}
			if len(q.Items) != 1 || q.Items[0].Issue != 53 {
				t.Fatalf("queue = %+v, want only issue 53", q.Items)
			}

			text := out.String()
			for _, want := range []string{
				"issue 52: intent failed lint:\n  unknown paths: docs/planner-notes.md\n" +
					"    Edit pkg/cli/intents.go, mirroring docs/planner-notes.md.\n",
				"lint passed (2 repository paths checked)",
			} {
				if strings.Contains(text, want) != hold {
					t.Errorf("hold=%v: output contains %q = %v:\n%s", hold, want, !hold, text)
				}
			}
			if !strings.Contains(text, "issue 52: parked needs-decision (intent failed lint: unknown paths)") ||
				!strings.Contains(text, "planned 1, parked 1") {
				t.Errorf("output:\n%s", text)
			}
		})
	}
}
