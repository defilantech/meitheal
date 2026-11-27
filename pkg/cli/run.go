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
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	sigsyaml "sigs.k8s.io/yaml"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent/repo"
	"github.com/defilantech/meitheal/pkg/agent/tools"
)

const (
	defaultDecisionsDir = ".meitheal/decisions"
	defaultLedgerPath   = ".meitheal/ledger.jsonl"
	// defaultGateDeadlineSeconds matches run_gate_job's own default.
	defaultGateDeadlineSeconds = 1800
	// notifyURLEnv is read when --notify-url is not given, so a URL that
	// carries a token need not sit in the process arguments.
	notifyURLEnv = "MEITHEAL_NOTIFY_URL"
)

type runOptions struct {
	queueFile    string
	decisionsDir string
	namespace    string
	coderAgent   string
	stallFactor  float64
	dryRun       bool

	templateFile        string
	parallel            string
	answerer            string
	notifyURL           string
	ledgerPath          string
	plan                bool
	once                bool
	gateHostPaths       string
	gatePriorityClasses string
	gateDeadline        int
	planner             intentsOptions
}

// newRunCommand drives a queue through the Meitheal pipeline against the
// cluster, parking judgment calls instead of blocking on them.
func newRunCommand() *cobra.Command {
	opts := &runOptions{}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive a queue of issues through Meitheal unattended",
		// The default directory is interpolated rather than spelled out, so
		// the help cannot drift from the flag.
		Long: fmt.Sprintf(`Drive a queue of issues through the Meitheal pipeline against the cluster.

The loop does the mechanical work (preflight skips, dispatch, watching for
stalls, independent verification, one feedback pass, finalizing a draft PR)
and parks judgment calls as files under %s for you to review with
'meitheal decisions'. It never blocks: a parked decision releases the lane
and the loop moves to the next item.

The queue comes from --queue (written by 'meitheal intents' or by hand) or
from --plan, which plans --issues first with the same planner flags as
'meitheal intents'. Workloads are built from --template, a YAML file with
the keys spec (a Workload spec: agents, gateProfile, maxReviewIterations),
gpuGate, gpuNode and assistedBy (the PR's attribution line). --coder-agent fills spec.coderAgentRef.

Items run in two lanes, cpu and gpu, at most --parallel of each at once,
and an item waits for every issue in its 'after' list to finish. A cpu-lane
branch that changes a file under --gpu-paths fails verify ("this change
touches GPU code and needs the GPU gate") and its feedback pass is verified
by the CPU gate and then the GPU gate.

Preflight skips a closed issue, which then counts as finished for the items
after it. An issue whose own task branch exists with no Workload of the
loop's behind it parks escalate ("the loop lost track of this issue's run")
until answered.

Every park is announced: a one-line comment on the issue, a label on the
draft PR, and a POST of the full decision to --notify-url (or $%s).
A park is answered by a comment from --answerer, one line:

  /meitheal requeue        start a new round with the same intent
  /meitheal decide <text>  plan it again with your decision, then a new round
  /meitheal drop           comment once and stop working on it

or with 'meitheal decisions answer', which also takes hand-fix. A new round
recreates the issue's Workload at attempt 1 on the same branch and draft
PR; an issue the loop never dispatched goes through preflight first. A
decide answer asks the planner again, so it needs --repo-dir and
--planner-url, with or without --plan.

Each drive's end is appended to --ledger (JSONL). On a restart, an item the
ledger shows done or dropped is not driven again, an item with a decision
file stays parked, and an item whose Workload exists resumes it. SIGINT or
SIGTERM stops the loop without parking or deleting anything.

The GitHub token is read from $GITHUB_TOKEN, else from
~/.config/meitheal/github-token on every request. See docs/run-loop.md.

Example:

  meitheal run --queue queue.yaml --template template.yaml \
    --coder-agent example-coder --answerer example-maintainer`, defaultDecisionsDir, notifyURLEnv),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.dryRun {
				return printRunPlan(cmd.OutOrStdout(), opts)
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runLive(ctx, cmd.OutOrStdout(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.queueFile, "queue", "", "Path to the work queue YAML (this or --plan)")
	f.StringVar(&opts.decisionsDir, "decisions-dir", defaultDecisionsDir, "Where parked decisions are written")
	f.StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace for Workloads")
	f.StringVar(&opts.coderAgent, "coder-agent", "", "Coder Agent name (required)")
	f.Float64Var(&opts.stallFactor, "stall-factor", DefaultStallFactor,
		"Kill a run with no branch after this multiple of baseline")
	f.BoolVar(&opts.dryRun, "dry-run", false, "Print what would happen without applying anything")
	f.StringVar(&opts.templateFile, "template", "",
		"Workload template YAML (spec, gpuGate, gpuNode, assistedBy); required for a live run")
	f.StringVar(&opts.parallel, "parallel", "cpu=1,gpu=1", "Items run at once per lane, as cpu=N,gpu=M")
	f.StringVar(&opts.answerer, "answerer", "", "The one GitHub login whose '/meitheal' comments answer a park")
	f.StringVar(&opts.notifyURL, "notify-url", "",
		"URL that receives a JSON POST for every park (default $"+notifyURLEnv+")")
	f.StringVar(&opts.ledgerPath, "ledger", defaultLedgerPath, "JSONL file each drive's end is appended to")
	f.BoolVar(&opts.plan, "plan", false, "Plan --issues with the planner first, then drive what was planned")
	f.BoolVar(&opts.once, "once", false,
		"Drive the queue until nothing more can start, then exit (default: keep reading answers every 5 minutes)")
	f.StringVar(&opts.gateHostPaths, "gate-allowed-host-paths", "",
		"Comma-separated host directories a gate profile's placement may mount; empty allows none")
	f.StringVar(&opts.gatePriorityClasses, "gate-allowed-priority-classes", "",
		"Comma-separated priority classes a gate profile's placement may request; empty allows none")
	f.IntVar(&opts.gateDeadline, "gate-deadline-seconds", defaultGateDeadlineSeconds,
		"Wall-clock limit of one gate Job, in seconds")
	addPlannerFlags(f, &opts.planner, " (with --plan)")
	// A queue-less run has nothing to do, and an agent-less run would have
	// to guess which box does the work.
	_ = cmd.MarkFlagRequired("coder-agent")
	cmd.MarkFlagsOneRequired("queue", "plan")
	cmd.MarkFlagsMutuallyExclusive("queue", "plan")
	return cmd
}

// printRunPlan loads the queue the human prepared, checks every intent is
// readable, and prints what a run would do. Nothing is applied.
func printRunPlan(w io.Writer, opts *runOptions) error {
	// Before the queue: a bad --stall-factor is a typo in the command that was
	// just typed, and reporting a queue problem first would have the human
	// editing a file when the fix is on the command line.
	if err := checkStallFactor(opts.stallFactor); err != nil {
		return err
	}
	if opts.plan {
		return fmt.Errorf("--plan calls the planner and parks decisions, so it cannot be a --dry-run; " +
			"run 'meitheal intents --hold' to see what it would plan")
	}
	q, intents, err := loadRunQueue(opts.queueFile)
	if err != nil {
		return err
	}
	// Labelled with the flag names that produced them, so the plan reads back
	// as the command that made it.
	fprintln(w, "dry run, nothing is applied.")
	fprintf(w, "queue: %s\n", opts.queueFile)
	fprintf(w, "coder-agent: %s\n", opts.coderAgent)
	fprintf(w, "namespace: %s\n", opts.namespace)
	fprintf(w, "decisions-dir: %s\n", opts.decisionsDir)
	fprintf(w, "stall-factor: %g\n", opts.stallFactor)
	fprintf(w, "items: %d\n", len(q.Items))
	for i, item := range q.Items {
		// The reserved name: nothing has dispatched, so there is no returned
		// Workload name to build the branch from.
		fprintf(w, "  issue %d (%s): branch %s, intent %s (%d bytes)\n",
			item.Issue, item.Repo, taskBranch(plannedWorkloadName(item.Issue), item.Issue),
			item.IntentPath, len(intents[i]))
	}
	return nil
}

// loadRunQueue reads a queue file and every intent it points at. A queue
// with no items is valid here, unlike for ParseQueue: a run on an empty
// queue still reads answers to the decisions already parked.
func loadRunQueue(path string) (Queue, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Queue{}, nil, fmt.Errorf("read queue: %w", err)
	}
	var probe Queue
	if err := sigsyaml.Unmarshal(data, &probe); err != nil {
		return Queue{}, nil, fmt.Errorf("parse queue: %w", err)
	}
	if len(probe.Items) == 0 {
		return Queue{Repo: probe.Repo}, nil, nil
	}
	q, err := ParseQueue(data)
	if err != nil {
		return Queue{}, nil, err
	}
	intents := make([]string, len(q.Items))
	for i, item := range q.Items {
		s, err := q.IntentFor(item)
		if err != nil {
			return Queue{}, nil, err
		}
		intents[i] = s
	}
	return q, intents, nil
}

// runLive wires the cluster, the forge and the planner into the run loop.
// Everything the human typed is checked before any of them is touched.
func runLive(ctx context.Context, w io.Writer, opts *runOptions) error {
	if err := checkStallFactor(opts.stallFactor); err != nil {
		return err
	}
	limits, err := ParseParallel(opts.parallel)
	if err != nil {
		return fmt.Errorf("--parallel: %w", err)
	}
	if opts.gateDeadline <= 0 {
		return fmt.Errorf("--gate-deadline-seconds must be greater than 0, got %d", opts.gateDeadline)
	}
	var q Queue
	if !opts.plan {
		if q, _, err = loadRunQueue(opts.queueFile); err != nil {
			return err
		}
	}
	if opts.templateFile == "" {
		return fmt.Errorf("--template is required for a live run: it is the Workload every item starts from")
	}
	tpl, err := LoadWorkloadTemplate(opts.templateFile)
	if err != nil {
		return err
	}
	if err := setCoderAgent(&tpl, opts.coderAgent); err != nil {
		return err
	}
	gpuPaths := splitList(opts.planner.gpuPaths)
	if len(gpuPaths) > 0 && tpl.GPUGate == nil {
		return fmt.Errorf("--gpu-paths moves a cpu-lane branch that touches them to the GPU gate, " +
			"but the template has no gpuGate")
	}
	runRepo, err := runRepository(opts, q)
	if err != nil {
		return err
	}
	var issues []int32
	if opts.plan {
		if issues, err = parseIssueList(opts.planner.issues); err != nil {
			return fmt.Errorf("--issues: %w", err)
		}
		if opts.planner.repoDir == "" || opts.planner.plannerURL == "" {
			return fmt.Errorf("--plan needs --repo-dir and --planner-url")
		}
	}
	// The planner answers decide and requeue on a planner park, with or
	// without --plan, once it has both a checkout and a model to ask.
	var plan PlanConfig
	var call PlannerCaller
	havePlanner := opts.planner.repoDir != "" && opts.planner.plannerURL != ""
	if (opts.planner.repoDir != "") != (opts.planner.plannerURL != "") {
		return fmt.Errorf("--repo-dir and --planner-url go together")
	}
	if havePlanner {
		if plan, call, err = opts.planner.plannerSetup(w); err != nil {
			return err
		}
	}
	if _, err := repo.TokenFromEnvOrFile(); err != nil {
		return fmt.Errorf("a GitHub token is needed in $%s or %s: %w",
			githubTokenEnv, githubTokenFile, err)
	}
	forge := NewGitHubRunForge(githubAPIBase, repo.TokenFromEnvOrFile)

	c, kcs, err := newRunClients()
	if err != nil {
		return err
	}
	gate := NewGateRunner(c, tools.RunGateJobToolConfig{
		Namespace:              opts.namespace,
		AllowedHostPaths:       splitList(opts.gateHostPaths),
		AllowedPriorityClasses: splitList(opts.gatePriorityClasses),
		ActiveDeadlineSeconds:  int32(min(opts.gateDeadline, math.MaxInt32)), //nolint:gosec // G115: clamped
		LogTailFn:              podLogTail(kcs),
	})
	notifyURL := opts.notifyURL
	if notifyURL == "" {
		notifyURL = os.Getenv(notifyURLEnv)
	}
	effects := NewClusterEffects(c, opts.namespace, forge, tpl, gate, ClusterEffectsOptions{
		NotifyURL: notifyURL, StallFactor: opts.stallFactor, GPUPaths: gpuPaths,
	})
	baseline, err := BaselineFor(ctx, c, opts.namespace, opts.coderAgent)
	if err != nil {
		return fmt.Errorf("read the coder's baseline from the audit records: %w", err)
	}

	queueFile := opts.queueFile
	items := q.Items
	if opts.plan {
		queueFile = filepath.Join(opts.planner.out, queueFileName)
		items, err = planForRun(ctx, w, forge, call, runRepo, issues, opts, plan)
		if err != nil {
			return err
		}
	}
	var replan replanFunc
	if havePlanner {
		replan = newReplanner(forge, call, runRepo, opts.planner.repoDir, opts.planner.out, opts.decisionsDir, plan)
	}
	return runQueueLoop(ctx, runLoopConfig{
		Effects:      effects,
		Forge:        forge,
		DecisionsDir: opts.decisionsDir,
		LedgerPath:   opts.ledgerPath,
		Answerer:     opts.answerer,
		Repo:         runRepo,
		Baseline:     baseline,
		Factor:       opts.stallFactor,
		Limits:       limits,
		Once:         opts.once,
		QueueFile:    queueFile,
		Replan:       replan,
	}, items)
}

// setCoderAgent fills the template's coder from --coder-agent, and refuses a
// template that already names a different one rather than pick silently.
func setCoderAgent(tpl *WorkloadTemplate, agent string) error {
	if ref := tpl.Spec.CoderAgentRef; ref != nil && ref.Name != "" && ref.Name != agent {
		return fmt.Errorf("--coder-agent is %q but the template's spec.coderAgentRef is %q", agent, ref.Name)
	}
	tpl.Spec.CoderAgentRef = &corev1.LocalObjectReference{Name: agent}
	return nil
}

// runRepository is the one repository the run works in: --repo, else the
// queue's, else its items'. checkOneRepo then refuses an item elsewhere.
func runRepository(opts *runOptions, q Queue) (string, error) {
	r := opts.planner.repo
	if r == "" {
		r = q.Repo
	}
	if r == "" && len(q.Items) > 0 {
		r = q.Items[0].Repo
	}
	if r == "" {
		return "", fmt.Errorf("the run needs a repository: --repo, or repo in the queue file")
	}
	return r, nil
}

// planForRun plans the issues that need it and returns the run's queue. On a
// restart (the queue file from the earlier --plan is there) an issue already
// in it keeps its intent, and an issue that is parked or finished is not
// planned again: planning a parked issue would refresh its park and lose the
// answers given to it. The merged queue is written back.
func planForRun(ctx context.Context, w io.Writer, f RunForge, call PlannerCaller, runRepo string,
	issues []int32, opts *runOptions, cfg PlanConfig) ([]QueueItem, error) {
	queuePath := filepath.Join(opts.planner.out, queueFileName)
	var kept []QueueItem
	if _, err := os.Stat(queuePath); err == nil {
		prev, _, err := loadRunQueue(queuePath)
		if err != nil {
			return nil, fmt.Errorf("the queue from an earlier --plan: %w", err)
		}
		for _, it := range prev.Items {
			if slices.Contains(issues, it.Issue) {
				kept = append(kept, it)
			}
		}
	}
	last, err := ReadLedger(opts.ledgerPath, runRepo)
	if err != nil {
		return nil, err
	}
	parked, err := parkedIssues(opts.decisionsDir, runRepo)
	if err != nil {
		return nil, err
	}
	var toPlan []int32
	for _, n := range issues {
		inQueue := slices.ContainsFunc(kept, func(it QueueItem) bool { return it.Issue == n })
		switch {
		case inQueue:
		case parked[n]:
			fprintf(w, "issue %d: parked already, not planned again\n", n)
		case ledgerFinished(last[n]):
			fprintf(w, "issue %d: finished already, not planned again\n", n)
		default:
			toPlan = append(toPlan, n)
		}
	}
	items := kept
	if len(toPlan) > 0 {
		q, err := planIssuesTo(ctx, f, call, runRepo, opts.planner.repoDir, toPlan, opts.planner.out,
			opts.decisionsDir, cfg)
		if err != nil {
			return nil, err
		}
		items = append(items, q.Items...)
	}
	keptSet := map[int32]bool{}
	for i := range items {
		items[i].Repo = runRepo
		keptSet[items[i].Issue] = true
	}
	// An earlier item may have run after an issue that is no longer queued.
	for i := range items {
		items[i].After = slices.DeleteFunc(items[i].After, func(a int32) bool { return !keptSet[a] })
	}
	if len(items) == 0 {
		fprintln(w, "nothing to drive from the plan")
		return nil, nil
	}
	if err := writeQueueFile(queuePath, Queue{Repo: runRepo, Items: items}); err != nil {
		return nil, err
	}
	fprintf(w, "queue: %s (%d item(s))\n", queuePath, len(items))
	return items, nil
}

// newRunClients builds the controller-runtime client the Effects use and the
// clientset the gate's pod-log tail reads through.
func newRunClients() (client.Client, kubernetes.Interface, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}
	if err := meithealv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, nil, fmt.Errorf("failed to register meitheal scheme: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, nil, err
	}
	kcs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return c, kcs, nil
}

// podLogTail fetches the end of a gate Job's pod log, the evidence a failed
// gate hands the feedback pass. Like the meitheal-agent's own helper
// (cmd/meitheal-agent makePodLogTailFn) it finds the Job's one pod by the
// modern job-name label or the legacy one, and anything unexpected yields "".
// Unlike it, it asks the API server for the END of the log (TailLines) and
// keeps the last MaxLogTailBytes of that: that is where a gate fails, and a
// large log is never read from the start.
func podLogTail(kcs kubernetes.Interface) func(ctx context.Context, namespace, jobName string) string {
	return func(ctx context.Context, namespace, jobName string) string {
		for _, selector := range []string{"batch.kubernetes.io/job-name=" + jobName, "job-name=" + jobName} {
			pods, err := kcs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err != nil || len(pods.Items) != 1 {
				continue
			}
			tail := int64(gateLogTailLines)
			stream, err := kcs.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name,
				&corev1.PodLogOptions{TailLines: &tail}).Stream(ctx)
			if err != nil {
				return ""
			}
			defer func() { _ = stream.Close() }()
			b, _ := io.ReadAll(io.LimitReader(stream, maxGateLogRead))
			return lastBytes(string(b), tools.MaxLogTailBytes)
		}
		return ""
	}
}

// gateLogTailLines is how many lines from the end of a gate log are fetched,
// and maxGateLogRead bounds the bytes read from them (a line can be long).
const (
	gateLogTailLines = 2000
	maxGateLogRead   = 8 << 20
)

// checkStallFactor refuses a stall budget that would kill every run.
//
// pflag parses the value with strconv.ParseFloat, which happily accepts "NaN",
// "Inf" and "-1", and IsStalled compares elapsed against factor x baseline: at
// zero or below the threshold is zero or negative and the first watch tick
// declares a run that has been going for a second stalled, and NaN converts to
// a Duration that every elapsed time is greater than, which does the same. The
// flag is the place to say so, because the human can see which flag is wrong
// before anything is dispatched. IsStalled defends itself as well, for callers
// that never went through this command.
func checkStallFactor(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return fmt.Errorf("--stall-factor must be a finite number greater than 0, got %g: "+
			"anything else kills every run on its first watch tick", f)
	}
	return nil
}

// newDecisionsCommand lists and answers parked decisions.
func newDecisionsCommand() *cobra.Command {
	dir := defaultDecisionsDir
	cmd := &cobra.Command{
		Use:   "decisions",
		Short: "List and answer decisions the run loop parked",
		// Without this a stray argument is silently ignored and the full
		// queue is listed, which reads as "nothing matched".
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A missing default directory means nothing has parked yet,
			// which is the normal state and not worth an error. A missing
			// directory the human typed is a typo, and answering a typo with
			// "No parked decisions." turns it into a confident "nothing to
			// do" while the real queue sits unread somewhere else.
			if cmd.Flags().Changed("decisions-dir") {
				if _, err := os.Stat(dir); err != nil {
					return fmt.Errorf("--decisions-dir %s: %w", dir, err)
				}
			}
			ds, err := ListDecisions(dir)
			// Render first. ListDecisions returns what it could parse
			// alongside an error naming what it could not, and this command
			// exists to show a human what is parked: one truncated file must
			// not hide the rest. Returning the error afterwards still exits
			// non-zero and names the file. Nothing is rendered when nothing
			// was listed, so "No parked decisions." never appears next to an
			// error and reads as reassurance.
			if len(ds) > 0 || err == nil {
				renderDecisions(cmd.OutOrStdout(), ds)
			}
			return err
		},
	}
	cmd.PersistentFlags().StringVar(&dir, "decisions-dir", defaultDecisionsDir, "Where parked decisions live")

	answer := &cobra.Command{
		Use:   "answer ISSUE KIND ANSWER",
		Short: "Answer one parked decision",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			// ParseInt, not Sscanf: Sscanf("1602x", "%d") succeeds with 1602
			// and no error, so a typo would silently answer a decision the
			// human did not name.
			issue, err := strconv.ParseInt(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("ISSUE must be a number: %w", err)
			}
			if err := AnswerDecision(dir, int32(issue), args[1], args[2]); err != nil {
				return err
			}
			// The parsed issue, not the argument: the confirmation should
			// name the decision that was actually written.
			fprintf(cmd.OutOrStdout(), "answered %d/%s: %s\n", issue, args[1], args[2])
			return nil
		},
	}
	cmd.AddCommand(answer)
	return cmd
}

// flattenCell keeps one table cell on one line. Reason arrives from verify
// output and stall evidence and Answer from a shell argument, so either can
// carry a newline or a tab, and either one shifts every row that follows it
// out of alignment. Every cell goes through it rather than only those two: a
// decision file is human-editable YAML, so any field can come back mangled.
func flattenCell(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// decisionOptions renders the answers a decision will accept. An unanswered
// decision that offers no options accepts anything, since AnswerDecision only
// enforces the list when there is one, and saying so beats a bare placeholder
// that reads as "nothing you can do here".
func decisionOptions(d Decision) string {
	if d.Answer != "" {
		return "-"
	}
	if len(d.Options) == 0 {
		return "any"
	}
	return strings.Join(d.Options, "|")
}

// orDash renders an empty cell as a dash. tabwriter pads an empty cell out to
// the column width, so an omitted value is indistinguishable from a row that
// has slipped a column; a dash says "nothing here" out loud.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// renderDecisions prints the parked-decision queue as a table.
//
// WORKLOAD is the only column that says which run to go and look at: ISSUE
// names the work, but a human wanting the logs, the branch or the transcript
// needs the Workload, and without the column the only way to get it is to open
// the YAML. OPTIONS is the column that makes the listing actionable: without it
// the only way to learn what an answer may be is to read the YAML or guess and
// wait for the rejection. REASON comes last because it is the one free-form
// column, and leading with it would push the actionable ones off the right
// of a narrow terminal.
//
// Decision.Stage is deliberately NOT a column. Every park the machine produces
// today already names its stage in REASON ("stalled" only comes from watch,
// both verify reasons say "verify"), so a STAGE column would spend width
// repeating what the row already says and push the actionable columns further
// right. The field stays in the YAML for anyone who needs it.
func renderDecisions(w io.Writer, ds []Decision) {
	if len(ds) == 0 {
		fprintln(w, "No parked decisions.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fprintln(tw, "ISSUE\tWORKLOAD\tKIND\tOPENED\tOPTIONS\tANSWER\tREASON")
	for _, d := range ds {
		fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.Issue, flattenCell(orDash(d.Workload)), flattenCell(d.Kind),
			d.Opened.Format("2006-01-02 15:04"),
			flattenCell(decisionOptions(d)), flattenCell(orDash(d.Answer)), flattenCell(d.Reason))
	}
	_ = tw.Flush()
}
