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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// contextFixture is a repository with two files about webhook retries and
// several that are not, including a binary and a symlink that mention the
// topic and must never be read.
func contextFixture(t *testing.T) repoListing {
	t.Helper()
	dir := gitRepo(t, map[string]string{
		"docs/retries.md": "# Webhook retries\n\nA failed webhook delivery is retried with exponential backoff.\n" +
			"The backoff doubles after each attempt, up to the retry cap.\n",
		"pkg/retry/backoff.go": "package retry\n\n// Backoff returns the delay before retry attempt n of a webhook\n" +
			"// delivery: exponential, capped at MaxBackoff.\nfunc Backoff(n int) int { return n }\n",
		// One incidental match: well under the topic files' score.
		"docs/install.md": "# Install\n\nDownload the release, then start the service with the install script.\n" +
			"If the download fails, the script retries once. Then check the service is up and serving requests.\n",
		"pkg/metrics/prom.go": "package metrics\n\n// Register adds the Prometheus collectors to the registry.\n" +
			"func Register() {}\n",
		"README.md": "# widgets\n\nA small service. See docs/install.md to get started.\n",
		"go.sum":    "example.com/webhook/retry/backoff v1.0.0 h1:abc=\n",
		"vendor/example.com/retry/backoff.go": "package retry\n\n// Webhook retry backoff, exponential, capped.\n" +
			"func Backoff() {}\n",
		"assets/logo.png": "PNG\x00webhook retries exponential backoff\x00",
	})
	if err := os.Symlink("retries.md", filepath.Join(dir, "docs", "retries-link.md")); err != nil {
		t.Fatal(err)
	}
	gitAdd(t, dir, "docs/retries-link.md")
	listing, err := repoFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return listing
}

func gitAdd(t *testing.T, dir string, paths ...string) {
	t.Helper()
	out, err := gitOutput(dir, append([]string{"add", "--"}, paths...)...)
	if err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
}

func relatedPaths(fs []namedFile) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Path)
	}
	return out
}

const (
	retryTitle = "Webhook retry backoff grows too fast"
	retryBody  = "Failed deliveries are retried with an exponential backoff that reaches the cap after two attempts."
)

func TestRelatedFiles_RanksTheIssueTopic(t *testing.T) {
	l := contextFixture(t)
	got := relatedFileContents(l.Root, l.Files, retryTitle, retryBody, nil, defaultContextBytes, defaultContextFiles)
	paths := relatedPaths(got)
	slices.Sort(paths)
	if want := []string{"docs/retries.md", "pkg/retry/backoff.go"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("related = %v, want %v", paths, want)
	}
	for _, f := range got {
		if f.Path == "docs/retries.md" && (!strings.Contains(f.Content, "doubles after each attempt") || f.Truncated) {
			t.Fatalf("docs/retries.md content = %q truncated=%v", f.Content, f.Truncated)
		}
	}
	again := relatedFileContents(l.Root, l.Files, retryTitle, retryBody, nil, defaultContextBytes, defaultContextFiles)
	if !reflect.DeepEqual(got, again) {
		t.Fatal("ranking is not deterministic")
	}
}

// A different topic in the same repository pulls in its own files.
func TestRelatedFiles_FollowsTheTopic(t *testing.T) {
	l := contextFixture(t)
	got := relatedPaths(relatedFileContents(l.Root, l.Files, "Install script fails",
		"Starting the service after download fails.", nil, defaultContextBytes, defaultContextFiles))
	if len(got) == 0 || got[0] != "docs/install.md" {
		t.Fatalf("related = %v, want docs/install.md first", got)
	}
	for _, p := range got {
		if strings.Contains(p, "retry") || strings.Contains(p, "retries") || p == "pkg/metrics/prom.go" {
			t.Errorf("related holds %s, which is not about installing", p)
		}
	}
}

func TestRelatedFiles_SkipsNamedFilesAndRespectsLimits(t *testing.T) {
	l := contextFixture(t)
	skip := map[string]bool{"docs/retries.md": true}
	got := relatedPaths(relatedFileContents(l.Root, l.Files, retryTitle, retryBody, skip,
		defaultContextBytes, defaultContextFiles))
	if !reflect.DeepEqual(got, []string{"pkg/retry/backoff.go"}) {
		t.Fatalf("related = %v, want the named file skipped", got)
	}

	one := relatedFileContents(l.Root, l.Files, retryTitle, retryBody, nil, defaultContextBytes, 1)
	if len(one) != 1 {
		t.Fatalf("got %d files with a limit of 1", len(one))
	}

	const budget = 40
	cut := relatedFileContents(l.Root, l.Files, retryTitle, retryBody, nil, budget, defaultContextFiles)
	total := 0
	for _, f := range cut {
		total += len(f.Content)
	}
	if len(cut) != 1 || total != budget || !cut[0].Truncated {
		t.Fatalf("with a %d-byte budget: %d files, %d bytes, truncated=%v", budget, len(cut), total,
			len(cut) > 0 && cut[0].Truncated)
	}

	for _, limits := range [][2]int{{0, defaultContextFiles}, {defaultContextBytes, 0}, {-1, 1}} {
		if got := relatedFileContents(l.Root, l.Files, retryTitle, retryBody, nil, limits[0], limits[1]); got != nil {
			t.Errorf("limits %v gave %v, want none", limits, relatedPaths(got))
		}
	}
}

// One ranked file never takes more than relatedFileCap, so a long file
// cannot crowd out the rest.
func TestRelatedFiles_PerFileCap(t *testing.T) {
	long := "# Retries\n" + strings.Repeat("webhook retry backoff ", relatedFileCap/10)
	dir := gitRepo(t, map[string]string{"docs/long.md": long, "docs/short.md": "webhook retry backoff\n"})
	l, err := repoFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := relatedFileContents(l.Root, l.Files, retryTitle, retryBody, nil, 4*relatedFileCap, defaultContextFiles)
	if len(got) != 2 {
		t.Fatalf("related = %v, want both", relatedPaths(got))
	}
	for _, f := range got {
		if f.Path == "docs/long.md" && (len(f.Content) != relatedFileCap || !f.Truncated) {
			t.Fatalf("long file: %d bytes truncated=%v, want %d and truncated", len(f.Content), f.Truncated,
				relatedFileCap)
		}
	}
}

func TestEachTerm(t *testing.T) {
	var got []string
	eachTerm("ParsePlanVerdict reads HTTPServer configs; the 2026 issue_title is in it", func(s string) {
		got = append(got, s)
	})
	want := []string{"parse", "plan", "verdict", "parseplanverdict", "read", "http", "server", "httpserver",
		"config", "issue", "title"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terms = %v, want %v", got, want)
	}
}

// The ranked files reach the prompt under their own heading, inside data
// blocks, and the prompt lists the files it holds.
func TestIntentPrompt_RelatedFilesFencedAndListed(t *testing.T) {
	in := intentPromptInput{
		Issue: 7, Title: "t", Body: "b",
		Named: []namedFile{{Path: "NAMED-PATH", Content: "NAMED-X"}},
		Related: []namedFile{
			{Path: "RELATED-PATH", Content: "RELATED-X"},
			{Path: "HEAD-PATH", Content: "HEAD-X", Truncated: true},
		},
		BashTimeoutSeconds: 900,
	}
	if !containsAny(in.untrustedText(), "RELATED-X") || !containsAny(in.untrustedText(), "RELATED-PATH") {
		t.Fatal("untrustedText leaves out the related files, so a nonce in one would not be refused")
	}
	p := buildIntentPrompt(in)
	n := promptNonce(t, p)
	open, end := "<<<UNTRUSTED-DATA nonce="+n+">>>", "<<<END nonce="+n+">>>"
	for _, want := range []string{"RELATED-X", "HEAD-X"} {
		i := strings.Index(p, want)
		if i < 0 {
			t.Fatalf("%s missing", want)
		}
		if strings.LastIndex(p[:i], open) <= strings.LastIndex(p[:i], end) {
			t.Errorf("%s is not inside a data block", want)
		}
	}
	if i, j := strings.Index(p, "NAMED-X"), strings.Index(p, relatedHeading); i < 0 || j < i ||
		j > strings.Index(p, "RELATED-X") {
		t.Error("the related files do not follow the named files under their own heading")
	}
	for _, want := range []string{
		seenFullHeading + "\n" + open + "\nNAMED-PATH\nRELATED-PATH\n" + end,
		seenHeadHeading + "\n" + open + "\nHEAD-PATH\n" + end,
		seenNote,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, seenNoneNote) {
		t.Error("prompt says it holds no contents")
	}

	none := buildIntentPrompt(intentPromptInput{Issue: 1, Title: "t", Body: "b", BashTimeoutSeconds: 900})
	if !strings.Contains(none, seenNoneNote) || strings.Contains(none, seenFullHeading) ||
		strings.Contains(none, relatedHeading) {
		t.Error("a prompt without contents does not say so")
	}
}

// Through PlanIssues: an issue that names no file gets the files about its
// topic and not the others; with no context budget it gets none.
func TestPlanIssues_PromptCarriesRelatedFiles(t *testing.T) {
	for _, budget := range []int{defaultContextBytes, 0} {
		e := newPlanEnv(t)
		l := contextFixture(t)
		e.repoDir = l.Root
		e.cfg.ContextBytes, e.cfg.ContextFiles = budget, defaultContextFiles
		f := newPlanForge(map[int32]IssueDetail{52: {Title: retryTitle, Body: retryBody}})
		s := &stubPlanner{answers: map[int32]string{52: plannedAnswer("cpu", "pkg/retry/backoff.go")}}
		e.plan(t, f, s.call, 52)
		p := s.prompts[52]
		for _, content := range []string{"The backoff doubles after each attempt", "func Backoff(n int)"} {
			if strings.Contains(p, content) != (budget > 0) {
				t.Errorf("budget %d: prompt holds %q = %v", budget, content, budget == 0)
			}
		}
		for _, unrelated := range []string{"start the service with the install script", "Prometheus collectors"} {
			if strings.Contains(p, unrelated) {
				t.Errorf("budget %d: prompt holds unrelated %q", budget, unrelated)
			}
		}
	}
}

// The named files come out of the same budget first.
func TestPlanIssues_NamedFilesShareTheContextBudget(t *testing.T) {
	e := newPlanEnv(t)
	l := contextFixture(t)
	e.repoDir = l.Root
	named, err := os.ReadFile(filepath.Join(l.Root, "docs", "retries.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Room for the named file and nothing more.
	e.cfg.ContextBytes, e.cfg.ContextFiles = len(named), defaultContextFiles
	f := newPlanForge(map[int32]IssueDetail{52: {Title: retryTitle, Body: retryBody + " See docs/retries.md."}})
	s := &stubPlanner{answers: map[int32]string{52: plannedAnswer("cpu", "pkg/retry/backoff.go")}}
	e.plan(t, f, s.call, 52)
	p := s.prompts[52]
	if !strings.Contains(p, "The backoff doubles") {
		t.Fatal("the named file is missing")
	}
	if strings.Contains(p, "File pkg/retry/backoff.go:") {
		t.Fatal("a related file was added past the budget the named file used")
	}
}

func TestPlannerSetup_ContextFlags(t *testing.T) {
	cmd := newIntentsCommand()
	for name, want := range map[string]string{"context-bytes": "65536", "context-files": "6"} {
		fl := cmd.Flags().Lookup(name)
		if fl == nil || fl.DefValue != want {
			t.Fatalf("--%s = %+v, want default %s", name, fl, want)
		}
	}
	o := &intentsOptions{plannerKwargs: defaultPlannerKwargs, plannerTimeout: 1, maxTokens: 1, bashTimeout: 1,
		contextBytes: 1000, contextFiles: 3}
	cfg, _, err := o.plannerSetup(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextBytes != 1000 || cfg.ContextFiles != 3 {
		t.Fatalf("cfg = %d bytes, %d files", cfg.ContextBytes, cfg.ContextFiles)
	}
	for _, bad := range []*intentsOptions{
		{plannerKwargs: defaultPlannerKwargs, plannerTimeout: 1, maxTokens: 1, bashTimeout: 1, contextBytes: -1},
		{plannerKwargs: defaultPlannerKwargs, plannerTimeout: 1, maxTokens: 1, bashTimeout: 1, contextFiles: -1},
	} {
		if _, _, err := bad.plannerSetup(nil); err == nil {
			t.Errorf("no error for %+v", bad)
		}
	}
}

// A term in a file's path counts beyond its words: of two files with the
// same words, the one whose path names the topic ranks first even when it
// sorts last by path.
func TestRelatedFiles_PathTermsBoost(t *testing.T) {
	dir := gitRepo(t, map[string]string{
		"docs/aaa-notes.md":   "Tuning notes for the sender.\n",
		"docs/zzz-backoff.md": "Tuning notes for the sender.\n",
		"docs/other.md":       "Unrelated text about logging.\n",
	})
	l, err := repoFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := relatedPaths(relatedFileContents(l.Root, l.Files, "Backoff tuning", "", nil, defaultContextBytes,
		defaultContextFiles))
	if len(got) == 0 || got[0] != "docs/zzz-backoff.md" {
		t.Fatalf("related = %v, want docs/zzz-backoff.md first", got)
	}
}

func TestQueryTerms_TitleCountsDouble(t *testing.T) {
	got := queryTerms("Retry backoff", "The backoff grows; deliveries fail.")
	want := map[string]float64{"retry": 2, "backoff": 2, "grow": 1, "delivery": 1,
		"fail": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query = %v, want %v", got, want)
	}
}

// A match in a short file outweighs the same match in a long one, so a long
// file with an incidental mention does not outrank a focused one.
func TestRelatedFiles_LengthNormalized(t *testing.T) {
	filler := strings.Repeat("logging output format levels rotation ", 200)
	dir := gitRepo(t, map[string]string{
		"docs/aaa-long.md":  "Backoff.\n" + filler,
		"docs/zzz-short.md": "Backoff.\n",
		"docs/other.md":     "Unrelated text about colours.\n",
	})
	l, err := repoFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := relatedPaths(relatedFileContents(l.Root, l.Files, "Backoff", "", nil, defaultContextBytes,
		defaultContextFiles))
	if len(got) == 0 || got[0] != "docs/zzz-short.md" {
		t.Fatalf("related = %v, want docs/zzz-short.md first", got)
	}
}
