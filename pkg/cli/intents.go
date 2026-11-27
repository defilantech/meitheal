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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"

	agentrepo "github.com/defilantech/meitheal/pkg/agent/repo"
)

// The verdicts a planner answer can carry.
const (
	VerdictPlanned       = "planned"
	VerdictNeedsDecision = ParkNeedsDecision
	VerdictNeedsSplit    = ParkNeedsSplit
)

// Reasons a planner park carries. "planner output unreadable" is what the
// public park comment says, so it is pinned by tests.
//
// Notify posts Decision.Reason on the public issue, so a planner park's
// Reason is always one of these fixed strings and never model text: the
// model's own reason goes to Evidence["planner_reason"].
const (
	reasonUnreadable      = "planner output unreadable"
	reasonCallFailed      = "planner call failed"
	reasonNeedsDecision   = "the planner found a decision only a maintainer can make"
	reasonNeedsSplit      = "the planner judged the issue too big for one run"
	evidencePlannerReason = "planner_reason"
)

const (
	// intentsStage names the planner in a decision file's Stage.
	intentsStage = "plan"
	// namedFilesCap bounds the file contents given to the planner.
	namedFilesCap = 60 * 1024
	// unreadableEvidenceCap bounds how much of an unreadable answer is kept
	// in the decision file for a human to look at.
	unreadableEvidenceCap     = 4 * 1024
	defaultBashTimeoutSeconds = 900
	queueFileName             = "queue.yaml"
)

// PlanVerdict is a planner answer. Intent is the Markdown after the header:
// the intent for planned, the child drafts for needs-split, any extra detail
// for needs-decision.
type PlanVerdict struct {
	Kind   string
	Lane   string
	Files  []string
	Intent string
	Reason string
}

var errUnreadable = errors.New("unreadable planner output")

func unreadable(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUnreadable, fmt.Sprintf(format, a...))
}

// ParsePlanVerdict reads a planner answer in the format the intent prompt
// states. It is strict on purpose: anything it cannot read exactly is an
// error, and the caller parks the issue rather than guess at an intent.
func ParsePlanVerdict(raw string) (PlanVerdict, error) {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	// A thinking model served without a reasoning parser puts its reasoning
	// in the content, sometimes without the opening tag. Everything through
	// the first </think> is dropped; reasoning that never closed has no
	// VERDICT line first and is unreadable.
	if !strings.HasPrefix(s, "VERDICT:") {
		if _, after, closed := strings.Cut(s, "</think>"); closed {
			s = strings.TrimSpace(after)
		}
	}
	if s == "" {
		return PlanVerdict{}, unreadable("empty answer")
	}
	first, rest, _ := strings.Cut(s, "\n")
	kind, ok := strings.CutPrefix(strings.TrimSpace(first), "VERDICT:")
	if !ok {
		return PlanVerdict{}, unreadable("first line is not a VERDICT line")
	}
	switch kind = strings.TrimSpace(kind); kind {
	case VerdictPlanned:
		return parsePlanned(rest)
	case VerdictNeedsDecision, VerdictNeedsSplit:
		return parseParkVerdict(kind, rest)
	default:
		return PlanVerdict{}, unreadable("unknown verdict %q", kind)
	}
}

// parsePlanned reads the fenced YAML header and the intent after it.
func parsePlanned(rest string) (PlanVerdict, error) {
	lines := strings.Split(strings.TrimLeft(rest, " \t\n"), "\n")
	if open := strings.TrimSpace(lines[0]); open != fence+"yaml" && open != fence {
		return PlanVerdict{}, unreadable("planned verdict without a fenced YAML block")
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == fence {
			closing = i
			break
		}
	}
	if closing < 0 {
		return PlanVerdict{}, unreadable("YAML block never closed")
	}
	var hdr struct {
		Lane  string   `yaml:"lane"`
		Files []string `yaml:"files"`
	}
	dec := yaml.NewDecoder(strings.NewReader(strings.Join(lines[1:closing], "\n")))
	dec.KnownFields(true)
	if err := dec.Decode(&hdr); err != nil {
		return PlanVerdict{}, unreadable("YAML block: %v", err)
	}
	switch hdr.Lane {
	case "", "cpu", "gpu":
	default:
		return PlanVerdict{}, unreadable("lane %q, want cpu or gpu", hdr.Lane)
	}
	if len(hdr.Files) == 0 {
		return PlanVerdict{}, unreadable("planned verdict lists no files")
	}
	files := make([]string, 0, len(hdr.Files))
	for _, f := range hdr.Files {
		f = strings.TrimSpace(f)
		if f == "" {
			return PlanVerdict{}, unreadable("blank entry in files")
		}
		f = path.Clean(f)
		if path.IsAbs(f) || f == ".." || strings.HasPrefix(f, "../") {
			return PlanVerdict{}, unreadable("file %q is outside the repository", f)
		}
		files = append(files, f)
	}
	intent := strings.TrimSpace(strings.Join(lines[closing+1:], "\n"))
	if intent == "" {
		return PlanVerdict{}, unreadable("planned verdict with an empty intent")
	}
	return PlanVerdict{Kind: VerdictPlanned, Lane: hdr.Lane, Files: files, Intent: intent}, nil
}

// parseParkVerdict reads the reason line and whatever follows it.
func parseParkVerdict(kind, rest string) (PlanVerdict, error) {
	first, after, _ := strings.Cut(strings.TrimLeft(rest, " \t\n"), "\n")
	reason, ok := strings.CutPrefix(strings.TrimSpace(first), "reason:")
	if !ok {
		return PlanVerdict{}, unreadable("%s verdict without a reason line", kind)
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		return PlanVerdict{}, unreadable("%s verdict with an empty reason", kind)
	}
	return PlanVerdict{Kind: kind, Reason: reason, Intent: strings.TrimSpace(after)}, nil
}

// PlanConfig is the repository-specific configuration of PlanIssues. Its
// zero value applies no repository rules and never forces the gpu lane.
type PlanConfig struct {
	// DecisionLabel parks an issue carrying it without calling the model.
	// Empty means no label does.
	DecisionLabel string
	// GPULabels and GPUPaths force the gpu lane: an issue with any of the
	// labels, or a planned file under any of the path prefixes.
	GPULabels []string
	GPUPaths  []string
	// Rules is inserted into the prompt verbatim under its own heading.
	Rules string
	// TrustedCommenters are the logins (matched case-insensitively) whose
	// comments reach the prompt; empty means every comment does. Anyone can
	// comment on a public issue.
	TrustedCommenters []string
	// BashTimeoutSeconds is the coder's bash time limit the prompt states;
	// zero means 900.
	BashTimeoutSeconds int
	// Progress, when set, gets one line per issue.
	Progress io.Writer
	// MaintainerDecisions maps an issue to a decision its maintainer made
	// (a '/meitheal decide' answer). It reaches that issue's prompt as
	// settled, and the decision label no longer parks the issue: the human
	// it was waiting for has decided. Nil adds nothing.
	MaintainerDecisions map[int32]string
}

// PlanIssues asks the planner for an intent for each issue, writes each
// intent to outDir/<issue>.intent.md and the queue to outDir/queue.yaml, and
// parks every issue it cannot plan as a decision in decisionsDir. Parking is
// per issue: a failed or unreadable model call parks that issue and the next
// one is planned. A forge or file-system failure, or a cancelled context,
// stops the run with an error.
func PlanIssues(ctx context.Context, f RunForge, call PlannerCaller, repo, repoDir string, issues []int32,
	outDir, decisionsDir string, cfg PlanConfig) (Queue, error) {
	q, err := planIssuesTo(ctx, f, call, repo, repoDir, issues, outDir, decisionsDir, cfg)
	if err != nil {
		return Queue{}, err
	}
	return q.Queue, writeQueue(q.outDir, q.Queue)
}

// plannedQueue is a planned queue and the absolute directory its intents
// were written to.
type plannedQueue struct {
	Queue
	outDir string
}

// planIssuesTo is PlanIssues without writing queue.yaml: the run loop
// re-plans one issue at a time and keeps its own queue file.
func planIssuesTo(ctx context.Context, f RunForge, call PlannerCaller, repo, repoDir string, issues []int32,
	outDir, decisionsDir string, cfg PlanConfig) (plannedQueue, error) {
	if err := checkIssueList(issues); err != nil {
		return plannedQueue{}, err
	}
	listing, err := repoFiles(repoDir)
	if err != nil {
		return plannedQueue{}, err
	}
	if cfg.BashTimeoutSeconds <= 0 {
		cfg.BashTimeoutSeconds = defaultBashTimeoutSeconds
	}
	cfg.GPUPaths = cleanPrefixes(cfg.GPUPaths)
	// Absolute, so `meitheal run` finds the intents from any directory.
	if outDir, err = filepath.Abs(outDir); err != nil {
		return plannedQueue{}, fmt.Errorf("--out: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return plannedQueue{}, fmt.Errorf("create --out: %w", err)
	}
	p := planner{f: f, call: call, repo: repo, listing: listing, outDir: outDir, decisionsDir: decisionsDir,
		cfg: cfg}
	q := Queue{Repo: repo}
	planned := map[int32][]string{}
	for _, issue := range issues {
		item, files, err := p.planOne(ctx, issue)
		if err != nil {
			return plannedQueue{}, err
		}
		if item == nil {
			continue
		}
		for _, prev := range q.Items {
			if sharesFile(planned[prev.Issue], files) {
				item.After = append(item.After, prev.Issue)
			}
		}
		planned[issue] = files
		q.Items = append(q.Items, *item)
	}
	return plannedQueue{Queue: q, outDir: outDir}, nil
}

type planner struct {
	f            RunForge
	call         PlannerCaller
	repo         string
	listing      repoListing
	outDir       string
	decisionsDir string
	cfg          PlanConfig
}

// planOne plans one issue. A nil item with a nil error means it was parked.
func (p planner) planOne(ctx context.Context, issue int32) (*QueueItem, []string, error) {
	d, err := p.f.FetchIssue(ctx, p.repo, issue)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch issue %d: %w", issue, err)
	}
	decided := strings.TrimSpace(p.cfg.MaintainerDecisions[issue])
	if decided == "" && p.cfg.DecisionLabel != "" && slices.Contains(d.Labels, p.cfg.DecisionLabel) {
		reason := fmt.Sprintf("labelled %q: a human decides this issue", p.cfg.DecisionLabel)
		return nil, nil, p.park(issue, ParkNeedsDecision, reason, nil)
	}
	comments, omitted := trustedComments(d.Comments, p.cfg.TrustedCommenters)
	prompt := buildIntentPrompt(intentPromptInput{
		Issue:              issue,
		Title:              d.Title,
		Body:               d.Body,
		Comments:           comments,
		OmittedComments:    omitted,
		Files:              p.listing.Files,
		Named:              namedFileContents(p.listing.Root, p.listing.Files, d.Body),
		Rules:              p.cfg.Rules,
		BashTimeoutSeconds: p.cfg.BashTimeoutSeconds,
		MaintainerDecision: decided,
	})
	raw, err := p.call(ctx, prompt)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}
	if errors.Is(err, errPlannerTruncated) {
		return nil, nil, p.park(issue, ParkNeedsDecision, reasonUnreadable, map[string]string{"planner": err.Error()})
	}
	if err != nil {
		return nil, nil, p.park(issue, ParkNeedsDecision, reasonCallFailed, map[string]string{"planner": err.Error()})
	}
	v, err := ParsePlanVerdict(raw)
	if err != nil {
		return nil, nil, p.park(issue, ParkNeedsDecision, reasonUnreadable, map[string]string{
			"planner": err.Error(),
			"output":  capString(raw, unreadableEvidenceCap),
		})
	}
	switch v.Kind {
	case VerdictNeedsDecision:
		ev := map[string]string{evidencePlannerReason: v.Reason}
		if v.Intent != "" {
			ev["detail"] = v.Intent
		}
		return nil, nil, p.park(issue, ParkNeedsDecision, reasonNeedsDecision, ev)
	case VerdictNeedsSplit:
		ev := map[string]string{evidencePlannerReason: v.Reason}
		if v.Intent != "" {
			ev["drafts"] = v.Intent
		}
		return nil, nil, p.park(issue, ParkNeedsSplit, reasonNeedsSplit, ev)
	}

	intentPath := filepath.Join(p.outDir, fmt.Sprintf("%d.intent.md", issue))
	if err := os.WriteFile(intentPath, []byte(v.Intent+"\n"), 0o644); err != nil {
		return nil, nil, fmt.Errorf("write intent for issue %d: %w", issue, err)
	}
	lane := v.Lane
	if lane == "" {
		lane = "cpu"
	}
	if p.forcesGPU(d.Labels, v.Files) {
		lane = "gpu"
	}
	p.progress("issue %d: planned, lane %s, intent %s\n", issue, lane, intentPath)
	return &QueueItem{Issue: issue, IntentPath: intentPath, Lane: lane}, v.Files, nil
}

// trustedComments keeps the comments by a trusted login and counts the rest.
// No trusted logins means every comment is kept.
func trustedComments(cs []IssueComment, trusted []string) ([]IssueComment, int) {
	if len(trusted) == 0 {
		return cs, 0
	}
	var kept []IssueComment
	for _, c := range cs {
		if slices.ContainsFunc(trusted, func(t string) bool { return strings.EqualFold(t, c.Author) }) {
			kept = append(kept, c)
		}
	}
	return kept, len(cs) - len(kept)
}

// cleanPrefixes cleans each GPU path prefix and keeps its trailing slash: a
// prefix ending in "/" matches only files under that directory, one without
// it is a plain string prefix ("crates/gpu" also matches "crates/gpu-extra/").
func cleanPrefixes(prefixes []string) []string {
	var out []string
	for _, pre := range prefixes {
		if strings.TrimSpace(pre) == "" {
			continue
		}
		c := path.Clean(strings.TrimSpace(pre))
		if c == "." {
			continue
		}
		if strings.HasSuffix(pre, "/") && !strings.HasSuffix(c, "/") {
			c += "/"
		}
		out = append(out, c)
	}
	return out
}

// forcesGPU is the configured lane rule.
func (p planner) forcesGPU(labels, files []string) bool {
	for _, l := range labels {
		if slices.Contains(p.cfg.GPULabels, l) {
			return true
		}
	}
	for _, f := range files {
		for _, prefix := range p.cfg.GPUPaths {
			if strings.HasPrefix(f, prefix) {
				return true
			}
		}
	}
	return false
}

// park writes a planner decision. Opened is set so PollAnswers can tell this
// park's answers from the issue's history.
func (p planner) park(issue int32, kind, reason string, evidence map[string]string) error {
	where, err := ParkDecision(p.decisionsDir, Decision{
		Repo:     p.repo,
		Issue:    issue,
		Stage:    intentsStage,
		Kind:     kind,
		Opened:   time.Now().UTC(),
		Reason:   reason,
		Evidence: evidence,
		Options:  optionsFor(kind),
	})
	if err != nil {
		return fmt.Errorf("park issue %d: %w", issue, err)
	}
	p.progress("issue %d: parked %s (%s): %s\n", issue, kind, reason, where)
	return nil
}

func (p planner) progress(format string, a ...any) {
	if p.cfg.Progress != nil {
		_, _ = fmt.Fprintf(p.cfg.Progress, format, a...)
	}
}

func sharesFile(a, b []string) bool {
	for _, f := range a {
		if slices.Contains(b, f) {
			return true
		}
	}
	return false
}

func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cutUTF8(s, n) + "\n" + truncatedMarker
}

// writeQueue writes outDir/queue.yaml after checking it parses back. With
// nothing planned there is no queue, and one left by an earlier run is
// removed so `meitheal run` cannot pick it up as this run's.
func writeQueue(outDir string, q Queue) error {
	p := filepath.Join(outDir, queueFileName)
	if len(q.Items) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale queue: %w", err)
		}
		return nil
	}
	b, err := sigsyaml.Marshal(q)
	if err != nil {
		return fmt.Errorf("marshal queue: %w", err)
	}
	if _, err := ParseQueue(b); err != nil {
		return fmt.Errorf("planned queue does not parse: %w", err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return fmt.Errorf("write queue: %w", err)
	}
	return nil
}

func checkIssueList(issues []int32) error {
	if len(issues) == 0 {
		return errors.New("no issues to plan")
	}
	seen := map[int32]bool{}
	for _, n := range issues {
		if n <= 0 {
			return fmt.Errorf("issue %d is not a positive number", n)
		}
		if seen[n] {
			return fmt.Errorf("issue %d is listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// repoListing is a checkout's top level and its tracked files.
type repoListing struct {
	Root  string
	Files []string
}

// repoFiles validates dir as a git checkout and lists its tracked files
// from the top level, whichever directory inside the checkout dir names.
func repoFiles(dir string) (repoListing, error) {
	if dir == "" {
		return repoListing{}, errors.New("--repo-dir is required")
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return repoListing{}, fmt.Errorf("--repo-dir %s is not a directory", dir)
	}
	top, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return repoListing{}, fmt.Errorf("--repo-dir %s is not a git checkout: %w", dir, err)
	}
	root := strings.TrimSpace(string(top))
	out, err := gitOutput(root, "ls-files", "-z")
	if err != nil {
		return repoListing{}, fmt.Errorf("list files in %s: %w", root, err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return repoListing{Root: root, Files: files}, nil
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// pathTokenRE matches the path-shaped words of an issue body.
var pathTokenRE = regexp.MustCompile(`[\w./-]+`)

// namedFileContents returns the tracked files the body mentions, in the
// order it first mentions them, with their contents capped at namedFilesCap
// bytes in total: the file that crosses the cap is cut there and marked, and
// later ones are left out. Unreadable and binary files are skipped, and so is
// anything that is not a regular file inside root: git tracks symlinks, and
// a tracked link to a file outside the checkout must not be read.
func namedFileContents(root string, files []string, body string) []namedFile {
	tracked := make(map[string]bool, len(files))
	for _, f := range files {
		tracked[f] = true
	}
	var out []namedFile
	seen := map[string]bool{}
	budget := namedFilesCap
	for _, tok := range pathTokenRE.FindAllString(body, -1) {
		if budget <= 0 {
			break
		}
		name := strings.TrimPrefix(strings.TrimRight(tok, "."), "./")
		if !tracked[name] || seen[name] {
			continue
		}
		seen[name] = true
		b, ok := readRegularFileInside(root, name)
		if !ok || bytes.IndexByte(b, 0) >= 0 {
			continue
		}
		nf := namedFile{Path: name, Content: string(b)}
		if len(b) > budget {
			nf.Content, nf.Truncated = cutUTF8(string(b), budget), true
		}
		budget -= len(nf.Content)
		out = append(out, nf)
		if nf.Truncated {
			break
		}
	}
	return out
}

// readRegularFileInside reads root/name only when the cleaned path is inside
// root, it is a regular file (not a symlink), and no directory on the way
// resolves outside root.
func readRegularFileInside(root, name string) ([]byte, bool) {
	full := filepath.Join(root, filepath.FromSlash(name))
	if rel, err := filepath.Rel(root, full); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, false
	}
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, false
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return nil, false
	}
	if rel, err := filepath.Rel(realRoot, realFull); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false
	}
	b, err := os.ReadFile(realFull)
	if err != nil {
		return nil, false
	}
	return b, true
}

// The command.

const (
	defaultIntentsOut    = ".meitheal/intents"
	plannerTokenEnv      = "MEITHEAL_PLANNER_TOKEN" //nolint:gosec // G101: an env var name, not a credential
	githubTokenEnv       = "GITHUB_TOKEN"           //nolint:gosec // G101: an env var name, not a credential
	githubAPIBase        = "https://api.github.com"
	defaultPlannerKwargs = `{"thinking":true}`
)

// githubTokenFile is where repo.TokenFromEnvOrFile looks when $GITHUB_TOKEN is
// empty, as the help spells it.
const githubTokenFile = "~/.config/meitheal/github-token" //nolint:gosec // G101: a path, not a credential

type intentsOptions struct {
	repo              string
	issues            string
	repoDir           string
	out               string
	hold              bool
	plannerURL        string
	plannerModel      string
	plannerKwargs     string
	plannerTimeout    time.Duration
	maxTokens         int
	decisionsDir      string
	rulesFile         string
	gpuLabels         string
	gpuPaths          string
	trustedCommenters string
	decisionLabel     string
	bashTimeout       int
}

func newIntentsCommand() *cobra.Command {
	o := &intentsOptions{}
	cmd := &cobra.Command{
		Use:   "intents",
		Short: "Plan issues into intents and a queue for 'meitheal run'",
		Long: fmt.Sprintf(`Ask a planner model to write a short intent for each issue, and write the
intents and a queue for 'meitheal run'.

For each issue the planner reads the title, body and comments, the files
tracked in --repo-dir, and the contents of the files the issue names. It
either writes an intent or hands the issue to a human: an issue it cannot
plan, labelled --decision-label, or whose answer cannot be read is parked as
a decision under --decisions-dir for 'meitheal decisions', and is never put
in the queue.

Rules that belong to one repository go in --rules-file; they reach the
planner verbatim, and override the general rules where the two conflict.
--gpu-labels and --gpu-paths put an issue in the gpu lane.
Anyone can comment on a public issue: --trusted-commenters limits the comments
the planner reads to those logins. All issue and repository text reaches the
planner fenced as data, never as instructions.

The planner token is read from $%s. The GitHub token is read from $%s,
else from %s, on every request.

Example:

  meitheal intents --repo example/widgets --issues 52,53 --repo-dir ../widgets \
    --planner-url http://planner:8000 --planner-model planner --rules-file rules.md`,
			plannerTokenEnv, githubTokenEnv, githubTokenFile),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIntents(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	f := cmd.Flags()
	addPlannerFlags(f, o, " (required)")
	f.BoolVar(&o.hold, "hold", false, "Write the intents and queue, then stop for review")
	f.StringVar(&o.decisionsDir, "decisions-dir", defaultDecisionsDir, "Where parked decisions are written")
	for _, name := range []string{"repo", "issues", "repo-dir", "planner-url"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// addPlannerFlags registers the planner flags 'meitheal intents' and
// 'meitheal run --plan' share, so the two cannot drift apart. need is
// appended to the usage of the flags a plan cannot do without.
func addPlannerFlags(f *pflag.FlagSet, o *intentsOptions, need string) {
	f.StringVar(&o.repo, "repo", "", "Repository as owner/name"+need)
	f.StringVar(&o.issues, "issues", "", "Comma-separated issue numbers to plan"+need)
	f.StringVar(&o.repoDir, "repo-dir", "", "A git checkout of the repository at its base branch"+need)
	f.StringVar(&o.out, "out", defaultIntentsOut, "Where the intents and queue.yaml are written")
	f.StringVar(&o.plannerURL, "planner-url", "", "OpenAI-compatible base URL of the planner model"+need)
	f.StringVar(&o.plannerModel, "planner-model", "", "Planner model name to send in the request")
	f.StringVar(&o.plannerKwargs, "planner-kwargs", defaultPlannerKwargs,
		"JSON object sent as chat_template_kwargs; empty or {} sends none")
	f.DurationVar(&o.plannerTimeout, "planner-timeout", 30*time.Minute, "Time limit for one planner call")
	f.IntVar(&o.maxTokens, "max-tokens", 16000, "max_tokens for one planner call")
	f.StringVar(&o.rulesFile, "rules-file", "", "File of repository-specific rules given to the planner verbatim")
	f.StringVar(&o.gpuLabels, "gpu-labels", "", "Comma-separated issue labels that put an issue in the gpu lane")
	f.StringVar(&o.gpuPaths, "gpu-paths", "",
		"Comma-separated path prefixes; a planned file starting with one puts the issue in the gpu lane. "+
			"End a prefix with / to match only files under that directory")
	f.StringVar(&o.trustedCommenters, "trusted-commenters", "",
		"Comma-separated GitHub logins (any case) whose comments the planner reads; empty means all")
	f.StringVar(&o.decisionLabel, "decision-label", "decision",
		"Issue label that parks an issue for a human without calling the planner; empty disables")
	f.IntVar(&o.bashTimeout, "bash-timeout-seconds", defaultBashTimeoutSeconds,
		"The coder's bash time limit, stated to the planner")
}

// plannerSetup checks the planner flags and builds the PlanConfig and the
// planner call they describe. progress receives one line per planned issue.
func (o *intentsOptions) plannerSetup(progress io.Writer) (PlanConfig, PlannerCaller, error) {
	kwargs, err := parsePlannerKwargs(o.plannerKwargs)
	if err != nil {
		return PlanConfig{}, nil, fmt.Errorf("--planner-kwargs: %w", err)
	}
	if o.maxTokens <= 0 {
		return PlanConfig{}, nil, fmt.Errorf("--max-tokens must be greater than 0, got %d", o.maxTokens)
	}
	if o.bashTimeout <= 0 {
		return PlanConfig{}, nil, fmt.Errorf("--bash-timeout-seconds must be greater than 0, got %d", o.bashTimeout)
	}
	if o.plannerTimeout <= 0 {
		return PlanConfig{}, nil, fmt.Errorf("--planner-timeout must be greater than 0, got %s", o.plannerTimeout)
	}
	cfg := PlanConfig{
		DecisionLabel:      o.decisionLabel,
		GPULabels:          splitList(o.gpuLabels),
		GPUPaths:           splitList(o.gpuPaths),
		TrustedCommenters:  splitList(o.trustedCommenters),
		BashTimeoutSeconds: o.bashTimeout,
		Progress:           progress,
	}
	if o.rulesFile != "" {
		b, err := os.ReadFile(o.rulesFile)
		if err != nil {
			return PlanConfig{}, nil, fmt.Errorf("--rules-file: %w", err)
		}
		cfg.Rules = string(b)
	}
	call := httpPlannerCallWithOptions(o.plannerURL, o.plannerModel, os.Getenv(plannerTokenEnv), PlannerCallOptions{
		MaxTokens:       o.maxTokens,
		Temperature:     0.2,
		TemplateKwargs:  kwargs,
		Timeout:         o.plannerTimeout,
		RejectTruncated: true,
	})
	return cfg, call, nil
}

func runIntents(ctx context.Context, w io.Writer, o *intentsOptions) error {
	issues, err := parseIssueList(o.issues)
	if err != nil {
		return fmt.Errorf("--issues: %w", err)
	}
	cfg, call, err := o.plannerSetup(w)
	if err != nil {
		return err
	}
	if _, err := agentrepo.TokenFromEnvOrFile(); err != nil {
		return fmt.Errorf("a GitHub token that can read the issues is needed in $%s or %s: %w",
			githubTokenEnv, githubTokenFile, err)
	}
	forge := NewGitHubRunForge(githubAPIBase, agentrepo.TokenFromEnvOrFile)
	if ctx == nil {
		ctx = context.Background()
	}
	q, err := PlanIssues(ctx, forge, call, o.repo, o.repoDir, issues, o.out, o.decisionsDir, cfg)
	if err != nil {
		return err
	}
	parked := len(issues) - len(q.Items)
	fprintf(w, "planned %d, parked %d (see 'meitheal decisions --decisions-dir %s')\n",
		len(q.Items), parked, o.decisionsDir)
	if len(q.Items) == 0 {
		fprintln(w, "nothing planned, so no queue was written.")
		return nil
	}
	qp := filepath.Join(o.out, queueFileName)
	if o.hold {
		fprintf(w, "held: review the intents in %s, then run 'meitheal run --queue %s'\n", o.out, qp)
		return nil
	}
	fprintf(w, "queue: %s\n", qp)
	return nil
}

// parseIssueList parses "52,53" into issue numbers: each positive, none
// repeated.
func parseIssueList(s string) ([]int32, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("no issues given")
	}
	var out []int32
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%q is not an issue number", part)
		}
		out = append(out, int32(n))
	}
	if err := checkIssueList(out); err != nil {
		return nil, err
	}
	return out, nil
}

// parsePlannerKwargs parses the --planner-kwargs JSON object. Empty and {}
// mean no kwargs.
func parsePlannerKwargs(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("want a JSON object: %w", err)
	}
	if m == nil {
		return nil, errors.New("want a JSON object, got null")
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// splitList splits a comma list, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
