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

package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
	"github.com/defilantech/meitheal/pkg/agent"
)

// gateScheme is the scheme the fake controller-runtime client uses for
// every gate-tool test. batchv1 is the only API the tool touches.
func gateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batchv1 scheme: %v", err)
	}
	return s
}

// pinName returns a deterministic NameFn so polling can look the Job
// up without having to scan.
func pinName(name string) func(string) string {
	return func(string) string { return name }
}

// argsJSON builds a runGateJobArgs payload mirroring what
// executor_native.go's buildDeterministicArgs produces for the gate
// Agent. The checks field is omitted so the tool falls through to
// DefaultGateChecks; renderGateJob is tested separately for explicit
// check lists.
func argsJSON(t *testing.T, repo, branch string) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"repo":    repo,
		"branch":  branch,
		"taskRef": map[string]string{"namespace": "default", "name": "gate"},
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return out
}

// flipStatusOnce watches for the Job to appear, then patches the named
// status field to the requested count and returns. Tests use it to
// drive the fake apiserver through a Succeeded / Failed transition
// shortly after the tool's Create call.
func flipStatusOnce(ctx context.Context, c client.Client, key types.NamespacedName, succeeded, failed int32) {
	for ctx.Err() == nil {
		var job batchv1.Job
		if err := c.Get(ctx, key, &job); err == nil {
			job.Status.Succeeded = succeeded
			job.Status.Failed = failed
			_ = c.Status().Update(ctx, &job)
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
}

// flipDeadlineOnce watches for the Job to appear, then marks it Failed
// with a Failed condition whose Reason is DeadlineExceeded -- the shape
// the Kubernetes controller sets when a Job outlives its
// activeDeadlineSeconds (#1748). Mirrors flipStatusOnce's polling so a
// test can drive the fake apiserver through the same terminal transition.
func flipDeadlineOnce(ctx context.Context, c client.Client, key types.NamespacedName) {
	for ctx.Err() == nil {
		var job batchv1.Job
		if err := c.Get(ctx, key, &job); err == nil {
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{{
				Type:    batchv1.JobFailed,
				Status:  corev1.ConditionTrue,
				Reason:  batchv1.JobReasonDeadlineExceeded,
				Message: "Job was active longer than specified deadline",
			}}
			_ = c.Status().Update(ctx, &job)
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
}

// --- Schema / Name --------------------------------------------------------

func TestRunGateJob_NameAndSchema(t *testing.T) {
	tool := RunGateJobTool{}
	if got := tool.Name(); got != "run_gate_job" {
		t.Errorf("Name(): got %q", got)
	}
	schema := tool.Schema()
	if schema.Name != "run_gate_job" {
		t.Errorf("Schema.Name: got %q", schema.Name)
	}
	if !strings.Contains(string(schema.Parameters), "repo") ||
		!strings.Contains(string(schema.Parameters), "branch") {
		t.Errorf("Schema.Parameters missing repo/branch keys: %s", schema.Parameters)
	}
}

// --- Argument validation --------------------------------------------------

func TestRunGateJob_RequiresClient(t *testing.T) {
	tool := &RunGateJobTool{}
	_, err := tool.Execute(context.Background(), argsJSON(t, "x/y", "b"))
	if err == nil || !strings.Contains(err.Error(), "Client") {
		t.Errorf("expected Client-required error; got %v", err)
	}
}

func TestRunGateJob_MissingRepo(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Errorf("expected repo-required error; got %v", err)
	}
}

func TestRunGateJob_MissingBranch(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"x/y"}`))
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Errorf("expected branch-required error; got %v", err)
	}
}

func TestRunGateJob_BadArgsJSON(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{not-json`))
	if err == nil || !strings.Contains(err.Error(), "bad args") {
		t.Errorf("expected bad-args error; got %v", err)
	}
}

// --- Submit + poll happy paths -------------------------------------------

func TestRunGateJob_SucceededProducesGATEPASS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "meitheal-gate-fake-pass"
	key := types.NamespacedName{Namespace: "meitheal-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(_ context.Context, _, name string) string {
				return "=== make test ===\nok  github.com/x/y\nGATE PASS\n"
			},
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "defilantech/LLMKube", "meitheal/issue-503"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Terminal {
		t.Errorf("Terminal: want true")
	}
	if res.Verdict != VerdictGatePass {
		t.Errorf("Verdict: want %s got %s", VerdictGatePass, res.Verdict)
	}
	if !strings.Contains(res.Summary, "passed") {
		t.Errorf("Summary should announce pass; got %q", res.Summary)
	}
	if got, _ := res.Extra["logTail"].(string); !strings.Contains(got, "GATE PASS") {
		t.Errorf("logTail should carry the gate output; got %q", got)
	}
	if got, _ := res.Extra["jobName"].(string); got != jobName {
		t.Errorf("Extra.jobName: want %s got %s", jobName, got)
	}

	// And the Job actually got created at the apiserver with the
	// expected name + namespace.
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist on apiserver: %v", err)
	}
	if job.Labels["app.kubernetes.io/name"] != "meitheal-gate" {
		t.Errorf("missing canonical label: %#v", job.Labels)
	}
}

func TestRunGateJob_FailedProducesGATEFAIL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "meitheal-gate-fake-fail"
	key := types.NamespacedName{Namespace: "meitheal-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 0, 1)

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(_ context.Context, _, _ string) string {
				return "FAIL  github.com/x/y\nGATE FAIL\n"
			},
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateFail {
		t.Errorf("Verdict: want %s got %s", VerdictGateFail, res.Verdict)
	}
	if got, _ := res.Extra["logTail"].(string); !strings.Contains(got, "GATE FAIL") {
		t.Errorf("logTail should carry failure marker; got %q", got)
	}
}

// TestRunGateJob_DeadlineExceededProducesGATEERROR is the regression test
// for #1748: a gate Job killed by its activeDeadlineSeconds must map to
// GATE-ERROR (retryable gate infrastructure problem), not GATE-FAIL
// (branch is bad). The summary names the deadline and, when the log tail
// carries a phase header, the phase the Job was in when it died.
func TestRunGateJob_DeadlineExceededProducesGATEERROR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "meitheal-gate-fake-deadline"
	key := types.NamespacedName{Namespace: "meitheal-system", Name: jobName}

	go flipDeadlineOnce(ctx, c, key)

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(_ context.Context, _, _ string) string {
				return "=== bite check (base=main) ===\nok\n" +
					"=== per-hunk mutation coverage (base=abc) ===\n"
			},
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateError {
		t.Errorf("Verdict: want %s got %s", VerdictGateError, res.Verdict)
	}
	if !strings.Contains(res.Summary, "deadline") {
		t.Errorf("Summary should name the deadline; got %q", res.Summary)
	}
	// The phase annotated onto the summary is the LAST "=== <phase> ==="
	// header in the tail, which is where the Job stalled.
	if !strings.Contains(res.Summary, "during per-hunk mutation coverage (base=abc)") {
		t.Errorf("Summary should carry the active phase; got %q", res.Summary)
	}
}

// --- Error paths ----------------------------------------------------------

func TestRunGateJob_PollTimeoutProducesGATEERROR(t *testing.T) {
	// No goroutine to flip status -- the poll loop should hit PollTimeout
	// and return GATE-ERROR with a "poll timeout" reason.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName("meitheal-gate-stuck"),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  50 * time.Millisecond,
			LogTailFn:    func(context.Context, string, string) string { return "(no logs)" },
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateError {
		t.Errorf("Verdict: want %s got %s", VerdictGateError, res.Verdict)
	}
	if got, _ := res.Extra["pollError"].(string); got == "" {
		t.Errorf("pollError should be set on timeout; got empty")
	}
}

func TestRunGateJob_DuplicateCreateProducesGATEERROR(t *testing.T) {
	// Seed the fake apiserver with a Job at the name we will pin. The
	// Create call inside Execute should fail with AlreadyExists; the
	// tool maps that to a GATE-ERROR terminal result rather than a
	// Go-level error.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	seeded := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "meitheal-gate-collide", Namespace: "meitheal-system"},
	}
	c := fake.NewClientBuilder().
		WithScheme(gateScheme(t)).
		WithObjects(seeded).
		Build()

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName("meitheal-gate-collide"),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  500 * time.Millisecond,
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateError {
		t.Errorf("Verdict: want %s got %s", VerdictGateError, res.Verdict)
	}
	if got, _ := res.Extra["reason"].(string); !strings.Contains(got, "create job") {
		t.Errorf("reason should mention create failure; got %q", got)
	}
	// Sanity check the AlreadyExists shape on the underlying client so
	// we are not relying on a brittle string comparison.
	if err := c.Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "meitheal-gate-collide", Namespace: "meitheal-system"},
	}); !apierrors.IsAlreadyExists(err) {
		t.Errorf("expected AlreadyExists; got %v", err)
	}
}

// --- Defaults + sanitization ---------------------------------------------

func TestApplyConfigDefaults_FillsEveryField(t *testing.T) {
	c := applyConfigDefaults(RunGateJobToolConfig{})
	if c.Namespace == "" || c.Image == "" || c.CloneURLBase == "" {
		t.Errorf("string defaults missing: %#v", c)
	}
	// PVCName is deliberately NOT defaulted (#1538). A hardcoded claim name
	// is what broke named agent pools: the chart creates the claim per agent
	// as meitheal-<agentKey>-gate-cache, so any fixed fallback names a claim
	// that need not exist. Empty means "no cache volume", which the renderer
	// honours, and the chart supplies the real name via --gate-cache-pvc.
	if c.PVCName != "" {
		t.Errorf("PVCName must not be defaulted, got %q", c.PVCName)
	}
	if c.ActiveDeadlineSeconds == 0 || c.TTLSecondsAfterFinished == 0 {
		t.Errorf("deadline defaults missing: %#v", c)
	}
	if c.PollInterval == 0 || c.PollTimeout == 0 {
		t.Errorf("poll defaults missing: %#v", c)
	}
	if c.NameFn == nil {
		t.Errorf("NameFn default missing")
	}

	// PollTimeout defaults to 2 * ActiveDeadlineSeconds so the Job's
	// own deadline always fires before ours.
	want := 2 * time.Duration(c.ActiveDeadlineSeconds) * time.Second
	if c.PollTimeout != want {
		t.Errorf("PollTimeout default: want %s got %s", want, c.PollTimeout)
	}
}

// TestDefaultGateChecks_ContainsTestChart asserts that the chart validation
// target is present in DefaultGateChecks so a future refactor cannot silently
// drop it. Regression for #1441: a chart-only change that broke every helm
// test still returned GATE-PASS because no chart check existed.
func TestDefaultGateChecks_ContainsTestChart(t *testing.T) {
	found := false
	for _, c := range DefaultGateChecks {
		if c == "test-chart" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DefaultGateChecks missing test-chart: %v", DefaultGateChecks)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"issue-503-foo": "issue-503-foo",
		"Issue 503 Foo": "issue-503-foo",
		"weird/path!!":  "weird-path",
		"":              "task",
		"--leading--":   "leading",
		"BIG_CAPS_NAME": "big-caps-name",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q): want %q got %q", in, want, got)
		}
	}
}

// --- Template rendering ---------------------------------------------------

// renderGateArgsForTest renders a gate Job from the subset of rendererInput
// fields the bite-check tests care about, filling in the same boilerplate
// every other render test in this file repeats verbatim (image, PVC,
// resource sizing, task labels). Returns the joined container Args string.
// Recognized keys: repo, branch, baseBranch, upstreamURL (all string),
// biteCheck (bool), checks ([]string).
func renderGateArgsForTest(t *testing.T, in map[string]any) string {
	t.Helper()
	rin := rendererInput{
		Name:                    "meitheal-gate-test",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/x",
		Checks:                  []string{"fmt", "test"},
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-test",
	}
	if v, ok := in["repo"].(string); ok {
		rin.Repo = v
	}
	if v, ok := in["branch"].(string); ok {
		rin.Branch = v
	}
	if v, ok := in["baseBranch"].(string); ok {
		rin.BaseBranch = v
	}
	if v, ok := in["biteCheck"].(bool); ok {
		rin.BiteCheck = v
	}
	if v, ok := in["upstreamURL"].(string); ok {
		rin.UpstreamURL = v
	}
	if v, ok := in["checks"].([]string); ok {
		rin.Checks = v
	}
	if v, ok := in["generic"].(bool); ok {
		rin.Generic = v
	}
	if v, ok := in["commands"].([]string); ok {
		rin.Commands = v
	}
	job, err := renderGateJob(rin)
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	return strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
}

// TestGateJobCopyrightCheckRunsAfterBaseResolution pins the ordering the
// copyright check depends on. The clone is `--depth 1` of the coder branch,
// so no base ref exists while the `make <check>` loop runs; a copyright check
// placed there with BASE_REF=origin/<base> fails every gate Job run instead of
// checking anything. It must therefore run after the merge base is resolved,
// and take that resolved value.
func TestGateJobCopyrightCheckRunsAfterBaseResolution(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo":        "defilantech/LLMKube",
		"branch":      "meitheal/issue-2298",
		"baseBranch":  "main",
		"upstreamURL": "https://github.com/defilantech/llmkube.git",
	})

	checkAt := strings.Index(args, "make check-copyright")
	if checkAt < 0 {
		t.Fatalf("the gate Job must run the copyright check:\n%s", args)
	}
	// The base-resolution block assigns MB from the merge base; the check has
	// to come after it, and must consume that resolved value.
	resolveAt := strings.Index(args, "MB=$(git merge-base HEAD")
	if resolveAt < 0 {
		t.Fatalf("the gate Job template no longer resolves a merge base; the ordering this test pins has moved:\n%s", args)
	}
	if checkAt < resolveAt {
		t.Errorf("the copyright check runs at byte %d, before base resolution at %d: the shallow clone has no base ref there",
			checkAt, resolveAt)
	}
	// The checks loop runs before base resolution, so the target must not be
	// in DefaultGateChecks, or the loop would run it there anyway.
	for _, c := range DefaultGateChecks {
		if c == "check-copyright" {
			t.Errorf("check-copyright must not be in DefaultGateChecks: the checks loop runs before base resolution")
		}
	}
}

// TestGateJobCopyrightCheckProbesForMergeBase pins the guard the freeform path
// needs. There MB is a base tip rather than a merge base, and a tree diff
// against a tip cannot answer "what did this branch add": it also lists files
// the base deleted, and every narrowed variant of it reported either the wrong
// files or none. So the Job probes before it runs, and skips loudly when the
// probe fails.
func TestGateJobCopyrightCheckProbesForMergeBase(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo":       "defilantech/LLMKube",
		"branch":     "meitheal/issue-2298",
		"baseBranch": "main",
	})

	probeAt := strings.Index(args, `CMB=$(git merge-base HEAD "$MB"`)
	if probeAt < 0 {
		t.Fatalf("the Job must probe $MB for a merge base before running the check:\n%s", args)
	}
	skipAt := strings.Index(args, "copyright header check skipped, $MB is not a merge base")
	if skipAt < 0 {
		t.Fatalf("the Job must skip loudly when $MB is not a merge base:\n%s", args)
	}
	checkAt := strings.Index(args, "make check-copyright")
	if checkAt < 0 {
		t.Fatalf("the gate Job must still run the copyright check on the upstream path:\n%s", args)
	}
	if !(probeAt < skipAt && skipAt < checkAt) {
		t.Errorf("order must be probe (%d), skip (%d), run (%d)", probeAt, skipAt, checkAt)
	}
	if !strings.Contains(args[checkAt:], `BASE_REF="$CMB"`) {
		t.Errorf("the check must run against the probed merge base, not $MB:\n%s", args[checkAt:])
	}
}

// TestGateJobBiteCheckUsesUpstreamMergeBase asserts that when upstreamURL is
// set, the bite check fetches BaseBranch from the canonical repo (not the
// fork) and diffs/reverts against the merge-base of HEAD and that ref, never
// against the fork's (possibly stale) base tip. Regression for #1259: a
// stale fork main manufactured compile failures in untouched packages and a
// false GATE-FAIL on a good branch.
func TestGateJobBiteCheckUsesUpstreamMergeBase(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "meitheal/x", "biteCheck": true,
		"upstreamURL": "https://github.com/defilantech/LLMKube.git",
	})
	for _, want := range []string{
		// The `--` end-of-options guard before "$UPSTREAM_URL" prevents a
		// crafted value starting with `-` from being read as a git option
		// (e.g. --upload-pack=..., argv smuggling).
		`git fetch --depth 100 -- "$UPSTREAM_URL" "+refs/heads/$BASE:refs/remotes/ubase/$BASE"`,
		`git merge-base HEAD "refs/remotes/ubase/$BASE"`,
		`git fetch --deepen=1000 origin "$BRANCH"`,
		`git fetch --deepen=1000 -- "$UPSTREAM_URL" "+refs/heads/$BASE:refs/remotes/ubase/$BASE"`,
		`git diff --name-only "$MB" HEAD`,
		`git checkout "$MB" -- "$f"`,
		`GATE-WARN: bite check skipped`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("rendered gate args missing %q", want)
		}
	}
	// The fork-tip revert must be GONE on the upstream path.
	if strings.Contains(args, `git checkout "origin/$BASE" -- "$f"`) {
		t.Error("bite check still contains the fork-tip revert literal; " +
			"production must be reverted to the merge-base ref only")
	}
}

// TestGateJobBiteCheckFallsBackToOriginWithoutUpstreamURL asserts the
// pre-#1259 origin-fetch path survives for freeform tasks that carry no repo
// slug/upstream URL: the base ref is fetched from the fork itself.
func TestGateJobBiteCheckFallsBackToOriginWithoutUpstreamURL(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "meitheal/x", "biteCheck": true,
	})
	if !strings.Contains(args, `git fetch --depth 1 origin "+refs/heads/$BASE:refs/remotes/origin/$BASE"`) {
		t.Error("origin fallback path missing when upstreamURL is empty")
	}
}

// TestGateJobBiteCheckSanityCap asserts the bite check skips (rather than
// fails) when the merge-base diff touches an implausible number of files,
// the signature of a bad base resolution rather than a real coder diff.
func TestGateJobBiteCheckSanityCap(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "meitheal/x", "biteCheck": true,
		"upstreamURL": "https://github.com/defilantech/LLMKube.git",
	})
	if !strings.Contains(args, `-gt 200`) {
		t.Error("sanity cap (200 files) missing from bite block")
	}
}

// TestRunGateJob_UpstreamURLValidation asserts Execute rejects an upstreamURL
// that could be smuggled into git argv as an option (leading `-`, unknown
// scheme) and accepts the legitimate shapes: https, git@, and empty (the
// origin-fallback path). Defense in depth on top of the template's `--`
// end-of-options guard (#1259 hardening).
func TestRunGateJob_UpstreamURLValidation(t *testing.T) {
	cases := []struct {
		name        string
		upstreamURL string
		wantErr     bool
	}{
		{"leading dash rejected", "--upload-pack=/tmp/evil", true},
		{"non-URL scheme rejected", "ext::sh -c evil", true},
		{"https accepted", "https://github.com/defilantech/LLMKube.git", false},
		{"git-at accepted", "git@github.com:defilantech/LLMKube.git", false},
		{"empty accepted (origin fallback)", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
			jobName := "meitheal-gate-upstream-" + sanitizeName(tc.name)
			key := types.NamespacedName{Namespace: "meitheal-system", Name: jobName}

			// Only the accepted cases reach the poll loop; the goroutine is
			// harmless for the rejected ones (nothing to flip, ctx cancels it).
			go flipStatusOnce(ctx, c, key, 1, 0)

			raw, err := json.Marshal(map[string]any{
				"repo": "defilantech/LLMKube", "branch": "meitheal/x",
				"upstreamURL": tc.upstreamURL,
				"taskRef":     map[string]string{"namespace": "default", "name": "gate"},
			})
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

			tool := &RunGateJobTool{
				Client: c,
				Cfg: RunGateJobToolConfig{
					NameFn:       pinName(jobName),
					PollInterval: 5 * time.Millisecond,
					PollTimeout:  2 * time.Second,
				},
			}
			_, err = tool.Execute(ctx, raw)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.upstreamURL) {
					t.Errorf("want error naming the bad upstreamURL %q; got %v", tc.upstreamURL, err)
				}
				return
			}
			if err != nil {
				t.Errorf("valid upstreamURL %q rejected: %v", tc.upstreamURL, err)
			}
		})
	}
}

func TestRenderGateJob_RendersChecksAndPVCMount(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-x",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-503",
		Checks:                  []string{"fmt", "vet", "test"},
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-503",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	if job.Name != "meitheal-gate-x" {
		t.Errorf("Name: %q", job.Name)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "golang:1.26" {
		t.Errorf("Image: %q", got)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(args, "make fmt") ||
		!strings.Contains(args, "make vet") ||
		!strings.Contains(args, "make test") {
		t.Errorf("Args missing one of the requested checks:\n%s", args)
	}
	if !strings.Contains(args, "defilantech/LLMKube") ||
		!strings.Contains(args, "meitheal/issue-503") {
		t.Errorf("Args missing repo/branch substitution:\n%s", args)
	}
	if got := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "meitheal-gate-cache" {
		t.Errorf("PVC claim name: %q", got)
	}
	if got := job.Labels["meitheal.dev/task-name"]; got != "gate-503" {
		t.Errorf("task-name label: %q", got)
	}
}

func TestRenderGateJob_GenericRunsCommandsNotMakeOrBiteCheck(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-py",
		Namespace:               "meitheal-system",
		Image:                   "python:3.13",
		Repo:                    "acme/widgets",
		Branch:                  "meitheal/issue-1",
		BiteCheck:               true, // must be ignored on the generic path
		Generic:                 true,
		Commands:                []string{"ruff check .", "pytest -q"},
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-1",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "python:3.13" {
		t.Errorf("Image: %q", got)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(args, "( ruff check . )") || !strings.Contains(args, "( pytest -q )") {
		t.Errorf("generic args missing the resolved commands:\n%s", args)
	}
	if strings.Contains(args, "make ") {
		t.Errorf("generic path must not run make targets:\n%s", args)
	}
	if strings.Contains(args, "bite check") {
		t.Errorf("generic path must not run the Go-specific bite check:\n%s", args)
	}
}

// TestRenderGateJob_PodSecurityContextFSGroup asserts the rendered gate Job
// carries a PodSecurityContext with fsGroup=100 so non-root gate images
// (e.g. USER 65534) can write under XDG_DATA_HOME=/cache/xdg on the RWX
// gate-cache PVC. Regression for #1055.
func TestRenderGateJob_PodSecurityContextFSGroup(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-fsgroup",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-1055",
		Checks:                  []string{"fmt"},
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-1055",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	psc := job.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatalf("PodSecurityContext must be set on the gate pod")
	}
	if psc.FSGroup == nil {
		t.Fatalf("PodSecurityContext.FSGroup must be set for non-root gate images to write XDG_DATA_HOME on the RWX PVC")
	}
	if *psc.FSGroup != 100 {
		t.Errorf("PodSecurityContext.FSGroup: want 100 got %d", *psc.FSGroup)
	}
}

// TestRenderGateJob_CloneURLOverride asserts the template branches
// correctly on the CloneURL field: when set, the git clone target is
// the override URL verbatim; when empty, the historical
// CloneURLBase + Repo + .git is preserved. The human-readable log
// line ("=== clone <repo> @ <branch> ===") always names Repo so an
// operator sees what was being verified regardless of where the
// branch physically lives. Regression for #528 part 2.
func TestRenderGateJob_CloneURLOverride(t *testing.T) {
	cases := []struct {
		name        string
		cloneURL    string
		mustHave    []string
		mustNotHave []string
	}{
		{
			name:     "override wins",
			cloneURL: "https://github.com/Defilan/LLMKube.git",
			mustHave: []string{
				`"https://github.com/Defilan/LLMKube.git"`,
				"=== clone defilantech/LLMKube @ meitheal/issue-510 ===",
			},
			mustNotHave: []string{
				`"https://github.com/defilantech/LLMKube.git"`,
			},
		},
		{
			name:     "empty falls back to CloneURLBase + Repo",
			cloneURL: "",
			mustHave: []string{
				`"https://github.com/defilantech/LLMKube.git"`,
				"=== clone defilantech/LLMKube @ meitheal/issue-510 ===",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, err := renderGateJob(rendererInput{
				Name:                    "meitheal-gate-x",
				Namespace:               "meitheal-system",
				Image:                   "golang:1.26",
				Repo:                    "defilantech/LLMKube",
				Branch:                  "meitheal/issue-510",
				Checks:                  []string{"fmt"},
				PVCName:                 "meitheal-gate-cache",
				ActiveDeadlineSeconds:   1800,
				TTLSecondsAfterFinished: 86400,
				CPURequest:              "2",
				CPULimit:                "4",
				MemRequest:              "4Gi",
				MemLimit:                "8Gi",
				CloneURLBase:            "https://github.com",
				CloneURL:                tc.cloneURL,
				TaskNamespace:           "default",
				TaskName:                "gate-510",
			})
			if err != nil {
				t.Fatalf("renderGateJob: %v", err)
			}
			args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
			for _, sub := range tc.mustHave {
				if !strings.Contains(args, sub) {
					t.Errorf("missing %q in rendered args:\n%s", sub, args)
				}
			}
			for _, sub := range tc.mustNotHave {
				if strings.Contains(args, sub) {
					t.Errorf("unwanted %q present in rendered args:\n%s", sub, args)
				}
			}
		})
	}
}

// TestRenderGateJob_BiteCheck_Enabled asserts the bite check phase is
// rendered when BiteCheck is true, and that it contains the key logic
// for detecting test files, reverting production, and checking test
// results. Regression for #799.
func TestRenderGateJob_BiteCheck_Enabled(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-bite",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// The bite check phase must be present.
	if !strings.Contains(args, "bite check") {
		t.Errorf("bite check phase missing from rendered args:\\n%s", args)
	}
	// Must detect changes by committed diff against the resolved merge-base
	// (#1259), NOT by git status: the gate Job clones a committed branch, so
	// the working tree is clean and git status would always be empty (the
	// round-3 fix).
	if !strings.Contains(args, `git diff --name-only "$MB" HEAD`) {
		t.Errorf("bite check must detect changes via committed diff against the merge-base:\\n%s", args)
	}
	if strings.Contains(args, "git status --porcelain") {
		t.Errorf("bite check must NOT use git status (clean tree on a committed clone):\\n%s", args)
	}
	// Must detect test files among the diff.
	if !strings.Contains(args, "_test\\.go") {
		t.Errorf("bite check must detect test files:\\n%s", args)
	}
	// Must revert ONLY production files to their merge-base version, keeping
	// the new test changes so they run against pre-change production.
	if !strings.Contains(args, `git checkout "$MB" -- "$f"`) {
		t.Errorf("bite check must revert production files to the merge-base version:\\n%s", args)
	}
	// Must NOT use the removed stash mechanism (no-op on a clean committed tree).
	if strings.Contains(args, "git stash") {
		t.Errorf("bite check must not use git stash (the round-3 fix removed it):\\n%s", args)
	}
	// Must restore branch state after the check.
	if !strings.Contains(args, "git checkout HEAD -- $prod_files") {
		t.Errorf("bite check must restore branch state via git checkout HEAD:\\n%s", args)
	}
	// Must run ONLY the changed test packages, never the whole module: a
	// module-wide `go test ./...` fails on unrelated packages (controller
	// envtest needs KUBEBUILDER_ASSETS, absent in the gate image) and the bite
	// check would misread that as biting -> false pass.
	if !strings.Contains(args, "test_pkgs=$(echo \"$test_files\" | xargs -n1 dirname | sort -u") {
		t.Errorf("bite check must scope tests to the changed packages:\\n%s", args)
	}
	if strings.Contains(args, "go test -count=1 -timeout=180s ./...") {
		t.Errorf("bite check must NOT run the whole module (./...); unrelated failures mask the signal:\\n%s", args)
	}
	// Infra anomalies must surface as a loud GATE-WARN skip, never a
	// GATE-ERROR/GATE-FAIL: only a genuine non-biting result blocks the gate
	// (#1259).
	if !strings.Contains(args, "GATE-WARN: bite check skipped") {
		t.Errorf("bite check must surface infra anomalies as GATE-WARN skips:\\n%s", args)
	}
	if strings.Contains(args, "GATE-ERROR") {
		t.Errorf("bite check must not produce GATE-ERROR; infra anomalies are a GATE-WARN skip (#1259):\\n%s", args)
	}
	// Must report non-biting tests as GATE FAIL.
	if !strings.Contains(args, "non-biting") {
		t.Errorf("bite check must report non-biting tests:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_Disabled asserts the bite check phase is
// absent when BiteCheck is false (the default). Regression for #799.
func TestRenderGateJob_BiteCheck_Disabled(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-no-bite",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               false,
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// The bite check phase must NOT be present.
	if strings.Contains(args, "bite check") {
		t.Errorf("bite check phase should not be present when BiteCheck is false:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_SkipsWhenNoTestFiles asserts the bite check
// phase includes logic to skip when no test files changed, preventing
// false rejections of PRs that add/modify no test files. Regression for #799.
func TestRenderGateJob_BiteCheck_SkipsWhenNoTestFiles(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-skip",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// Must have the skip logic for no test files.
	if !strings.Contains(args, "no test files changed, skipping") {
		t.Errorf("bite check must skip when no test files changed:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_SkipsWhenNoProdFiles asserts the bite check
// phase includes logic to skip when no production files changed, preventing
// false rejections of PRs that only add tests. Regression for #799.
func TestRenderGateJob_BiteCheck_SkipsWhenNoProdFiles(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-skip-prod",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// Must have the skip logic for no production files.
	if !strings.Contains(args, "no production files changed, skipping") {
		t.Errorf("bite check must skip when no production files changed:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_FetchesBaseRef asserts the origin-fallback path
// (no upstreamURL) fetches the base ref with an explicit refspec (the clone
// is shallow + single-branch, so a plain fetch would not create
// origin/$BASE) and skips loudly (GATE-WARN, #1259), rather than failing the
// gate, when the base ref cannot be established. Regression for the round-3
// fix.
func TestRenderGateJob_BiteCheck_FetchesBaseRef(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-base",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-799",
		BaseBranch:              "main",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// Must fetch the base ref with an explicit refspec so origin/$BASE resolves.
	if !strings.Contains(args, `git fetch --depth 1 origin "+refs/heads/$BASE:refs/remotes/origin/$BASE"`) {
		t.Errorf("bite check must fetch base with an explicit refspec:\\n%s", args)
	}
	// Must verify the ref resolved and skip loudly otherwise.
	if !strings.Contains(args, `git rev-parse --verify --quiet "origin/$BASE"`) {
		t.Errorf("bite check must verify the base ref resolved:\\n%s", args)
	}
	if !strings.Contains(args, "could not fetch base ref") {
		t.Errorf("bite check must skip loudly when the base ref cannot be fetched:\\n%s", args)
	}
}

// TestRenderGateJob_BaseBranchEnv asserts the BASE_BRANCH env var is rendered
// from the BaseBranch field and that an empty BaseBranch defaults to main.
func TestRenderGateJob_BaseBranchEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"explicit", "release-1.0", "release-1.0"},
		{"defaults to main", "", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := renderGateJob(rendererInput{
				Name:                    "meitheal-gate-baseenv",
				Namespace:               "meitheal-system",
				Image:                   "golang:1.26",
				Repo:                    "defilantech/LLMKube",
				Branch:                  "meitheal/issue-799",
				BaseBranch:              tc.in,
				Checks:                  []string{"fmt", "test"},
				BiteCheck:               true,
				PVCName:                 "meitheal-gate-cache",
				ActiveDeadlineSeconds:   1800,
				TTLSecondsAfterFinished: 86400,
				CPURequest:              "2",
				CPULimit:                "4",
				MemRequest:              "4Gi",
				MemLimit:                "8Gi",
				CloneURLBase:            "https://github.com",
				TaskNamespace:           "default",
				TaskName:                "gate-799",
			})
			if err != nil {
				t.Fatalf("renderGateJob: %v", err)
			}
			var got, gotRef string
			found, foundRef := false, false
			for _, e := range job.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "BASE_BRANCH" {
					got = e.Value
					found = true
				}
				if e.Name == "BASE_REF" {
					gotRef = e.Value
					foundRef = true
				}
			}
			if !found {
				t.Fatalf("BASE_BRANCH env var not rendered")
			}
			if got != tc.want {
				t.Errorf("BASE_BRANCH = %q, want %q", got, tc.want)
			}
			// check-copyright needs the resolved merge base, so it is run after
			// base resolution rather than through the checks loop; no env var
			// carries it.
			if foundRef {
				t.Errorf("BASE_REF env var = %q; the copyright check takes its base from the resolved merge base", gotRef)
			}
		})
	}
}

// TestRunGateJob_ImageOverride asserts that a non-empty per-call image
// in the args overrides cfg.Image in the rendered Job, and that an
// empty per-call image falls back to cfg.Image. Regression for #839.
func TestRunGateJob_ImageOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []struct {
		name    string
		argsImg string
		cfgImg  string
		wantImg string
	}{
		{
			name:    "args image overrides cfg image",
			argsImg: "python:3.13",
			cfgImg:  "golang:1.26",
			wantImg: "python:3.13",
		},
		{
			name:    "empty args image falls back to cfg image",
			argsImg: "",
			cfgImg:  "golang:1.26",
			wantImg: "golang:1.26",
		},
		{
			name:    "empty args image falls back to cfg custom image",
			argsImg: "",
			cfgImg:  "rust:1",
			wantImg: "rust:1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
			jobName := "meitheal-gate-img-" + tc.name
			key := types.NamespacedName{Namespace: "meitheal-system", Name: jobName}

			go flipStatusOnce(ctx, c, key, 1, 0)

			args := map[string]any{
				"repo":   "defilantech/LLMKube",
				"branch": "meitheal/issue-839",
				"image":  tc.argsImg,
				"taskRef": map[string]string{
					"namespace": "default",
					"name":      "gate-839",
				},
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

			tool := &RunGateJobTool{
				Client: c,
				Cfg: RunGateJobToolConfig{
					Image:        tc.cfgImg,
					NameFn:       pinName(jobName),
					PollInterval: 5 * time.Millisecond,
					PollTimeout:  2 * time.Second,
				},
			}

			res, err := tool.Execute(ctx, argsJSON)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Verdict != VerdictGatePass {
				t.Fatalf("Verdict: want %s got %s", VerdictGatePass, res.Verdict)
			}

			// Verify the Job was created with the expected image.
			var job batchv1.Job
			if err := c.Get(ctx, key, &job); err != nil {
				t.Fatalf("Job should exist: %v", err)
			}
			if got := job.Spec.Template.Spec.Containers[0].Image; got != tc.wantImg {
				t.Errorf("Image: want %q got %q", tc.wantImg, got)
			}
		})
	}
}

func TestGateJobNamePreservesUniquenessSuffixWhenTruncated(t *testing.T) {
	// A long task name whose "meitheal-gate-<task>" already exceeds the 63-char
	// k8s object-name limit. The #768 validation caught a retry's gate Job
	// colliding with the prior attempt's because the trailing unix-ms
	// disambiguator was truncated away, so the retry could not re-gate and a
	// failing branch landed as GO. The suffix must survive truncation.
	long := "validate-1110-envtestloop-validate-1110-envtestloop"
	n1 := gateJobName(long, 1784157000000)
	n2 := gateJobName(long, 1784157000001)

	if len(n1) > 63 {
		t.Fatalf("name exceeds the 63-char k8s limit: len=%d %q", len(n1), n1)
	}
	if !strings.HasPrefix(n1, "meitheal-gate-") {
		t.Fatalf("lost the meitheal-gate- prefix: %q", n1)
	}
	if !strings.HasSuffix(n1, "-1784157000000") {
		t.Fatalf("uniqueness suffix was truncated away: %q", n1)
	}
	if n1 == n2 {
		t.Fatalf("two submissions of the same long task name collide: %q", n1)
	}
}

// TestRenderGateJob_MultiLineCommandStillUnmarshals covers #1293. A gate
// command is a plain string field and multi-line shell is the natural way to
// write a real check, but the template interpolates it inside a YAML block
// scalar. Before the fix a newline ended the block and the whole Job failed to
// unmarshal with "could not find expected ':'", so the gate never ran and the
// operator saw a YAML error citing a template they never wrote.
func TestRenderGateJob_MultiLineCommandStillUnmarshals(t *testing.T) {
	multi := "if grep -q needle file.txt; then\n  echo 'found'\n  exit 1\nfi\necho ok"
	args := renderGateArgsForTest(t, map[string]any{
		"generic":  true,
		"commands": []string{multi},
	})
	for _, want := range []string{"if grep -q needle file.txt; then", "echo 'found'", "exit 1", "echo ok"} {
		if !strings.Contains(args, want) {
			t.Errorf("rendered args lost %q:\n%s", want, args)
		}
	}
	if !strings.Contains(args, "...") {
		t.Errorf("multi-line banner should be truncated with an ellipsis:\n%s", args)
	}
}

// TestRenderGateJob_SingleLineCommandUnchanged guards the common case: a
// one-line command renders exactly as before, with no ellipsis added.
func TestRenderGateJob_SingleLineCommandUnchanged(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"generic":  true,
		"commands": []string{"go build ./..."},
	})
	if !strings.Contains(args, `echo "=== go build ./... ==="`) {
		t.Errorf("single-line command should render unchanged:\n%s", args)
	}
	if strings.Contains(args, "go build ./... ...") {
		t.Error("single-line command must not gain an ellipsis")
	}
}

// TestGateJobInstallsHelmOnlyForChartChecks pins the #1441 fix. The gate image
// is a plain golang image with no helm, so rendering the chart check without
// installing helm turns every gate run into "helm: command not found". The
// install must appear when a chart check is requested and must NOT appear
// otherwise, so Go-only branches do not pay for a download they cannot use.
func TestGateJobInstallsHelmOnlyForChartChecks(t *testing.T) {
	render := func(checks []string) string {
		job, err := renderGateJob(rendererInput{
			Name: "g", Namespace: "meitheal-system", Image: "golang:1.26",
			Repo: "o/r", Branch: "b", BaseBranch: "main", Checks: checks,
			PVCName: "meitheal-gate-cache", CloneURLBase: "https://github.com",
			ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 86400,
			CPURequest: "1", CPULimit: "2", MemRequest: "1Gi", MemLimit: "2Gi",
			HelmVersion: helmVersionFor(checks), HelmUnittestVersion: HelmUnittestVersion,
		})
		if err != nil {
			t.Fatalf("renderGateJob: %v", err)
		}
		return strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	}

	withChart := render([]string{"test", ChartCheck})
	if !strings.Contains(withChart, "install helm "+HelmVersion) {
		t.Error("chart check requested but the helm install is missing; the gate image has no helm")
	}
	// The plugin is fetched as a pinned tarball rather than via
	// `helm plugin install`, mirroring CI; assert on the pinned URL.
	if !strings.Contains(withChart, "unittest-"+HelmUnittestVersion+".tgz") {
		t.Errorf("helm-unittest tarball not pinned to %s", HelmUnittestVersion)
	}
	// platformHooks must be stripped or the 3.x plugin loader rejects the
	// plugin and the next `helm plugin list` hard-fails.
	if !strings.Contains(withChart, "platformHooks") {
		t.Error("plugin.yaml platformHooks strip is missing; helm 3.x will reject the plugin")
	}

	goOnly := render([]string{"fmt", "vet", "test"})
	if strings.Contains(goOnly, "install helm") {
		t.Error("helm installed for a Go-only run; the install must be conditional")
	}
}

// TestNeedsHelm covers the predicate directly.
func TestNeedsHelm(t *testing.T) {
	if !needsHelm([]string{"fmt", ChartCheck}) {
		t.Error("needsHelm should be true when the chart check is present")
	}
	if !needsHelm([]string{"fmt", MetalAgentRBACCheck}) {
		t.Error("needsHelm should be true when the metal-agent RBAC check is present; it renders the chart")
	}
	if needsHelm([]string{"fmt", "vet", "lint", "test"}) {
		t.Error("needsHelm should be false without a chart check")
	}
	if !needsHelm(DefaultGateChecks) {
		t.Error("DefaultGateChecks must include the chart check (#1441)")
	}
}

// TestGateJobInstallsJQOnlyForB200Harness pins the #1833 fix. The gate image
// is a plain golang image whose buildpack-deps chain installs no jq, while the
// B200 harness self-test normalizes benchmark JSON with it, so rendering the
// self-test check without installing jq turns every gate run into "required
// command not found: jq". The install must appear when the harness check is
// requested and must NOT appear otherwise.
func TestGateJobInstallsJQOnlyForB200Harness(t *testing.T) {
	render := func(checks []string) string {
		job, err := renderGateJob(rendererInput{
			Name: "g", Namespace: "meitheal-system", Image: "golang:1.26",
			Repo: "o/r", Branch: "b", BaseBranch: "main", Checks: checks,
			PVCName: "meitheal-gate-cache", CloneURLBase: "https://github.com",
			ActiveDeadlineSeconds: 1800, TTLSecondsAfterFinished: 86400,
			CPURequest: "1", CPULimit: "2", MemRequest: "1Gi", MemLimit: "2Gi",
			HelmVersion: helmVersionFor(checks), HelmUnittestVersion: HelmUnittestVersion,
			JQVersion: jqVersionFor(checks),
		})
		if err != nil {
			t.Fatalf("renderGateJob: %v", err)
		}
		return strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	}

	withHarness := render([]string{"fmt", "vet", B200HarnessCheck})
	if !strings.Contains(withHarness, "install jq "+JQVersion) {
		t.Error("harness check requested but the jq install is missing; the gate image has no jq")
	}
	if !strings.Contains(withHarness, "jq-linux-amd64") {
		t.Error("jq install must fetch the pinned static binary")
	}

	goOnly := render([]string{"fmt", "vet", "test"})
	if strings.Contains(goOnly, "install jq") {
		t.Error("jq installed for a Go-only run; the install must be conditional")
	}
}

// TestNeedsJQ covers the predicate directly.
func TestNeedsJQ(t *testing.T) {
	if !needsJQ([]string{"fmt", B200HarnessCheck}) {
		t.Error("needsJQ should be true when the harness check is present")
	}
	if needsJQ([]string{"fmt", "vet", "lint", "test"}) {
		t.Error("needsJQ should be false without the harness check")
	}
	if !needsJQ(DefaultGateChecks) {
		t.Error("DefaultGateChecks must include the B200 harness check (#1833)")
	}
}

// TestGateJobCacheVolumeOmittedWhenPVCNameEmpty asserts that an empty PVCName
// renders NO cache volume and NO /cache volumeMount.
//
// Regression for #1538. The field's contract has always said "empty disables
// the volume mount", but the template rendered `claimName: {{ .PVCName }}`
// unconditionally and the config defaulted an empty name back to a hardcoded
// "meitheal-gate-cache". With named agent pools that claim does not exist, so
// the Job mounted a PVC nobody created: the pod never binds, never schedules,
// and the Job dies at activeDeadlineSeconds looking like a slow gate rather
// than a misconfigured one.
func TestGateJobCacheVolumeOmittedWhenPVCNameEmpty(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-nocache",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-1538",
		Checks:                  []string{"fmt"},
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		PVCName:                 "",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	if n := len(job.Spec.Template.Spec.Volumes); n != 0 {
		t.Errorf("empty PVCName must render no volumes, got %d: %+v",
			n, job.Spec.Template.Spec.Volumes)
	}
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/cache" {
			t.Errorf("empty PVCName must render no /cache volumeMount, got %+v", m)
		}
	}
}

// TestGateJobCacheVolumeUsesGivenPVCName is the positive half: a set PVCName
// mounts exactly that claim, and never a hardcoded fallback (#1538).
func TestGateJobCacheVolumeUsesGivenPVCName(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "meitheal-gate-cached",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-1538",
		Checks:                  []string{"fmt"},
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		PVCName:                 "meitheal-multirepo-gate-cache",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	vols := job.Spec.Template.Spec.Volumes
	if len(vols) != 1 || vols[0].PersistentVolumeClaim == nil ||
		vols[0].PersistentVolumeClaim.ClaimName != "meitheal-multirepo-gate-cache" {
		t.Fatalf("want the per-agent claim mounted, got %+v", vols)
	}
	var found bool
	for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/cache" {
			found = true
		}
	}
	if !found {
		t.Error("a set PVCName must render the /cache volumeMount")
	}
}

func placementTestInput() rendererInput {
	return rendererInput{
		Name:                    "meitheal-gate-x",
		Namespace:               "meitheal-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "meitheal/issue-503",
		Checks:                  []string{"fmt", "vet", "test"},
		PVCName:                 "meitheal-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-503",
	}
}

// TestRenderGateJob_NilPlacement_Unchanged compares the rendered Job against a
// golden captured from the renderer before Placement existed, proving a nil
// placement leaves the Job byte for byte as it was.
func TestRenderGateJob_NilPlacement_Unchanged(t *testing.T) {
	in := placementTestInput()
	in.Placement = nil
	job, err := renderGateJob(in)
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	got, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want, err := os.ReadFile("testdata/gate_job_nil_placement.golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
		t.Errorf("nil-placement Job differs from golden.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func gpuPlacement() *meithealv1alpha1.GatePlacement {
	return &meithealv1alpha1.GatePlacement{
		NodeSelector: map[string]string{"kubernetes.io/hostname": "gpu-node-1"},
		Tolerations: []corev1.Toleration{
			{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		},
		Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
		},
		PriorityClassName: "batch",
		Env:               []corev1.EnvVar{{Name: "DLUTH_INKLING2_MINI", Value: "/models/inkling2-mini"}},
		Volumes: []corev1.Volume{{
			Name:         "models",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/models"}},
		}},
		VolumeMounts: []corev1.VolumeMount{{Name: "models", MountPath: "/models", ReadOnly: true}},
	}
}

func TestRenderGateJob_PlacementApplied(t *testing.T) {
	in := placementTestInput()
	in.Placement = gpuPlacement()
	in.AllowedHostPaths = []string{"/srv/models"}
	in.AllowedPriorityClasses = []string{"batch"}
	job, err := renderGateJob(in)
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]

	if pod.NodeSelector["kubernetes.io/hostname"] != "gpu-node-1" {
		t.Errorf("nodeSelector: %v", pod.NodeSelector)
	}
	if len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != "nvidia.com/gpu" ||
		pod.Tolerations[0].Operator != corev1.TolerationOpExists ||
		pod.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("tolerations: %+v", pod.Tolerations)
	}
	if pod.PriorityClassName != "batch" {
		t.Errorf("priorityClassName: %q", pod.PriorityClassName)
	}
	if gpu := c.Resources.Limits["nvidia.com/gpu"]; gpu.String() != "1" {
		t.Errorf("gpu limit: %v", c.Resources.Limits)
	}
	assertQuantities(t, "limit", c.Resources.Limits, map[corev1.ResourceName]string{"cpu": "4", "memory": "8Gi"})
	assertQuantities(t, "request", c.Resources.Requests, map[corev1.ResourceName]string{"cpu": "2", "memory": "4Gi"})
	var envOK bool
	for _, e := range c.Env {
		if e.Name == "DLUTH_INKLING2_MINI" && e.Value == "/models/inkling2-mini" {
			envOK = true
		}
	}
	if !envOK {
		t.Errorf("env missing: %+v", c.Env)
	}
	var modelsVol, cacheVol bool
	for _, v := range pod.Volumes {
		if v.Name == "models" && v.HostPath != nil {
			modelsVol = true
		}
		if v.Name == "cache" && v.PersistentVolumeClaim != nil {
			cacheVol = true
		}
	}
	if !modelsVol || !cacheVol {
		t.Errorf("volumes: models=%v cache=%v (%+v)", modelsVol, cacheVol, pod.Volumes)
	}
	var modelsMount, cacheMount bool
	for _, m := range c.VolumeMounts {
		if m.Name == "models" && m.MountPath == "/models" && m.ReadOnly {
			modelsMount = true
		}
		if m.Name == "cache" && m.MountPath == "/cache" {
			cacheMount = true
		}
	}
	if !modelsMount || !cacheMount {
		t.Errorf("mounts: models=%v cache=%v (%+v)", modelsMount, cacheMount, c.VolumeMounts)
	}
}

func TestRenderGateJob_PlacementResourcesOverrideByName(t *testing.T) {
	in := placementTestInput()
	in.Placement = &meithealv1alpha1.GatePlacement{
		Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"memory": resource.MustParse("32Gi")},
		},
	}
	job, err := renderGateJob(in)
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	r := job.Spec.Template.Spec.Containers[0].Resources
	if got := r.Limits["memory"]; got.String() != "32Gi" {
		t.Errorf("memory limit: %q", got.String())
	}
	if got := r.Limits["cpu"]; got.String() != "4" {
		t.Errorf("cpu limit: %q", got.String())
	}
	if got := r.Requests["memory"]; got.String() != "4Gi" {
		t.Errorf("memory request: %q", got.String())
	}
	if got := r.Requests["cpu"]; got.String() != "2" {
		t.Errorf("cpu request: %q", got.String())
	}
}

func assertQuantities(t *testing.T, kind string, got corev1.ResourceList, want map[corev1.ResourceName]string) {
	t.Helper()
	for name, w := range want {
		q := got[name]
		if q.String() != w {
			t.Errorf("default %s %s: got %q want %q", name, kind, q.String(), w)
		}
	}
}

// placementRejections lists one violation per validateGatePlacement rule.
func placementRejections() map[string]*meithealv1alpha1.GatePlacement {
	vol := func(src corev1.VolumeSource) corev1.Volume { return corev1.Volume{Name: "v", VolumeSource: src} }
	hp := func(path string) corev1.Volume {
		return vol(corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path}})
	}
	return map[string]*meithealv1alpha1.GatePlacement{
		"env valueFrom": {Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}}},
		"env collides with gate env": {Env: []corev1.EnvVar{{Name: "GOMODCACHE", Value: "/x"}}},
		"volume other source": {
			Volumes: []corev1.Volume{vol(corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}})},
		},
		"hostPath root":              {Volumes: []corev1.Volume{hp("/")}},
		"hostPath outside allowlist": {Volumes: []corev1.Volume{hp("/etc")}},
		"hostPath dotdot":            {Volumes: []corev1.Volume{hp("/srv/models/../../etc")}},
		"hostPath sibling prefix":    {Volumes: []corev1.Volume{hp("/srv/models-evil")}},
		"pvc not readOnly": {Volumes: []corev1.Volume{vol(corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "c"}})}},
		"mount of unknown volume": {VolumeMounts: []corev1.VolumeMount{{Name: "nope", MountPath: "/m", ReadOnly: true}}},
		"mount not readOnly": {
			Volumes:      []corev1.Volume{hp("/srv/models")},
			VolumeMounts: []corev1.VolumeMount{{Name: "v", MountPath: "/m"}},
		},
		"toleration empty key":       {Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}}},
		"priority class not allowed": {PriorityClassName: "system-node-critical"},
		"resources claims":           {Resources: &corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "c"}}}},
	}
}

func TestRenderGateJob_PlacementRejected(t *testing.T) {
	for name, p := range placementRejections() {
		t.Run(name, func(t *testing.T) {
			in := placementTestInput()
			in.Placement = p
			in.AllowedHostPaths = []string{"/srv/models"}
			in.AllowedPriorityClasses = []string{"batch"}
			if _, err := renderGateJob(in); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestRenderGateJob_PlacementEmptyAllowlistsAllowNone(t *testing.T) {
	in := placementTestInput()
	in.Placement = &meithealv1alpha1.GatePlacement{PriorityClassName: "batch"}
	if _, err := renderGateJob(in); err == nil {
		t.Error("priority class must be rejected with an empty allowlist")
	}
	in.Placement = gpuPlacement()
	if _, err := renderGateJob(in); err == nil {
		t.Error("hostPath must be rejected with an empty allowlist")
	}
}

func jobCount(t *testing.T, c client.Client) int {
	t.Helper()
	var l batchv1.JobList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	return len(l.Items)
}

// A model-supplied placement in the tool-call JSON is refused and no Job is created.
func TestRunGateJob_RejectsPlacementInModelArgs(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	for _, key := range []string{"placement", "Placement"} {
		raw := json.RawMessage(`{"repo":"x/y","branch":"b","` + key + `":{"volumes":[{"name":"r","hostPath":{"path":"/"}}]}}`)
		res, err := tool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if res == nil || res.Verdict != VerdictGateError {
			t.Errorf("%s: want GATE-ERROR result, got %+v", key, res)
		}
		if n := jobCount(t, c); n != 0 {
			t.Errorf("%s: %d Job(s) created", key, n)
		}
	}
	if strings.Contains(string(tool.Schema().Parameters), "placement") {
		t.Error("placement must not be advertised in the schema")
	}
}

// A placement from the trusted context that violates policy yields an error
// result and no Job.
func TestRunGateJob_InvalidContextPlacementCreatesNoJob(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	ctx := agent.WithGatePlacement(context.Background(), &meithealv1alpha1.GatePlacement{PriorityClassName: "batch"})
	res, err := tool.Execute(ctx, json.RawMessage(`{"repo":"x/y","branch":"b"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res == nil || res.Verdict != VerdictGateError {
		t.Errorf("want GATE-ERROR, got %+v", res)
	}
	if n := jobCount(t, c); n != 0 {
		t.Errorf("%d Job(s) created", n)
	}
}

// The gate pod runs the branch's own code: it must carry no API token and no
// Service environment variables, with or without a placement.
func TestRenderGateJob_NoTokenNoServiceLinks(t *testing.T) {
	for name, placement := range map[string]*meithealv1alpha1.GatePlacement{"nil": nil, "gpu": gpuPlacement()} {
		in := placementTestInput()
		in.Placement = placement
		in.AllowedHostPaths = []string{"/srv/models"}
		in.AllowedPriorityClasses = []string{"batch"}
		job, err := renderGateJob(in)
		if err != nil {
			t.Fatalf("%s: renderGateJob: %v", name, err)
		}
		pod := job.Spec.Template.Spec
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("%s: automountServiceAccountToken = %v, want false", name, pod.AutomountServiceAccountToken)
		}
		if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
			t.Errorf("%s: enableServiceLinks = %v, want false", name, pod.EnableServiceLinks)
		}
	}
}
