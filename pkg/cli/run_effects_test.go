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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

const (
	effNS    = "meitheal-system"
	effRepo  = "defilantech/dluth"
	effIssue = int32(42)
	gpuNode  = "gpu-box-1"
)

func effScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := meithealv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newEffectsClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(effScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&meithealv1alpha1.Workload{}, &meithealv1alpha1.AgenticTask{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func i32p(v int32) *int32 { return &v }
func boolp(v bool) *bool  { return &v }

// testTemplate is a Dlúth-shaped template: a generic CPU gate with the four
// gate commands, and a GPU gate placed on gpuNode.
func testTemplate() WorkloadTemplate {
	return WorkloadTemplate{
		Spec: meithealv1alpha1.WorkloadSpec{
			Intent:              "template placeholder",
			Repo:                effRepo,
			CoderAgentRef:       &corev1.LocalObjectReference{Name: "dluth-coder"},
			ReviewerAgentRefs:   []corev1.LocalObjectReference{{Name: "dluth-reviewer"}},
			MaxReviewIterations: i32p(2),
			MCPEnabled:          boolp(false),
			GateProfile: &meithealv1alpha1.GateProfile{
				Language: meithealv1alpha1.GateLanguageGeneric,
				Image:    "rust:1",
				Commands: meithealv1alpha1.GateCommands{
					Format: "cargo fmt --all -- --check",
					Lint:   "cargo clippy --all-targets -- -D warnings",
					Build:  "cargo build --release",
					Test:   "cargo test && bash tools/mutants.sh cpu",
				},
			},
		},
		GPUGate: &meithealv1alpha1.GateProfile{
			Language: meithealv1alpha1.GateLanguageGeneric,
			Image:    "dluth-cuda:1",
			Commands: meithealv1alpha1.GateCommands{
				Test: "cargo test -p dluth-cuda && bash tools/mutants.sh all",
			},
			Placement: &meithealv1alpha1.GatePlacement{
				NodeSelector: map[string]string{"kubernetes.io/hostname": gpuNode},
			},
		},
		GPUNode: gpuNode,
	}
}

func effItem(lane string) QueueItem {
	return QueueItem{Issue: effIssue, Repo: effRepo, IntentPath: "intents/42.md", Lane: lane}
}

// --- fakes -----------------------------------------------------------------

type forgeComment struct {
	repo  string
	issue int32
	body  string
}

// fakeRunForge records every write and answers PR lookups from prs.
type fakeRunForge struct {
	mu       sync.Mutex
	openPR   string
	branches map[string]bool
	prs      map[string]string // branch -> PR URL
	prErr    error
	comments []forgeComment
	bodies   map[string]string   // branch -> body
	labels   map[string][]string // branch -> labels
}

func newFakeRunForge() *fakeRunForge {
	return &fakeRunForge{
		branches: map[string]bool{},
		prs:      map[string]string{},
		bodies:   map[string]string{},
		labels:   map[string][]string{},
	}
}

func (f *fakeRunForge) OpenPRForIssue(_ context.Context, _ string, _ int32) (string, error) {
	return f.openPR, nil
}

func (f *fakeRunForge) BranchExists(_ context.Context, _, branch string) (bool, error) {
	return f.branches[branch], nil
}

func (f *fakeRunForge) CommentIssue(_ context.Context, repo string, issue int32, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, forgeComment{repo, issue, body})
	return nil
}

func (f *fakeRunForge) CommentsSince(context.Context, string, int32, time.Time) ([]IssueComment, error) {
	return nil, nil
}

func (f *fakeRunForge) PRForBranch(_ context.Context, _, branch string) (string, error) {
	if f.prErr != nil {
		return "", f.prErr
	}
	return f.prs[branch], nil
}

func (f *fakeRunForge) UpdatePRBody(_ context.Context, _, branch, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prs[branch] == "" {
		return fmt.Errorf("no open PR for %s", branch)
	}
	f.bodies[branch] = body
	return nil
}

func (f *fakeRunForge) LabelPR(_ context.Context, _, branch, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prs[branch] == "" {
		return fmt.Errorf("no open PR for %s", branch)
	}
	f.labels[branch] = append(f.labels[branch], label)
	return nil
}

func (f *fakeRunForge) FetchIssue(context.Context, string, int32) (IssueDetail, error) {
	return IssueDetail{}, nil
}

type gateCall struct {
	repo, branch string
	profile      meithealv1alpha1.GateProfile
}

// fakeGateRunner answers per gate image so a test can make the CPU or the
// GPU gate fail independently.
type fakeGateRunner struct {
	mu    sync.Mutex
	calls []gateCall
	dirty map[string]string // image -> failing log
}

func (g *fakeGateRunner) Run(
	_ context.Context, repo, branch string, p meithealv1alpha1.GateProfile,
) (bool, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, gateCall{repo, branch, p})
	if log, ok := g.dirty[p.Image]; ok {
		return false, log, nil
	}
	return true, "all gate checks passed", nil
}

func fastOpts() ClusterEffectsOptions {
	return ClusterEffectsOptions{Poll: time.Millisecond, GPURetry: time.Millisecond, GPUGiveUp: time.Hour}
}

func getWorkload(t *testing.T, c client.Client, name string) *meithealv1alpha1.Workload {
	t.Helper()
	var w meithealv1alpha1.Workload
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: effNS, Name: name}, &w); err != nil {
		t.Fatalf("get workload %s: %v", name, err)
	}
	return &w
}

// --- LoadWorkloadTemplate --------------------------------------------------

func TestLoadWorkloadTemplate_ReadsSpecGPUGateAndGPUNode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "template.yaml")
	body := `spec:
  intent: placeholder
  repo: defilantech/dluth
  coderAgentRef: {name: dluth-coder}
  maxReviewIterations: 2
  mcpEnabled: false
  gateProfile:
    language: generic
    commands:
      format: cargo fmt --all -- --check
gpuGate:
  language: generic
  commands:
    test: cargo test -p dluth-cuda && bash tools/mutants.sh all
gpuNode: gpu-box-1
`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tpl, err := LoadWorkloadTemplate(p)
	if err != nil {
		t.Fatalf("LoadWorkloadTemplate: %v", err)
	}
	if tpl.Spec.Repo != effRepo || tpl.Spec.CoderAgentRef == nil || tpl.Spec.CoderAgentRef.Name != "dluth-coder" {
		t.Errorf("spec not read: %+v", tpl.Spec)
	}
	if tpl.Spec.MaxReviewIterations == nil || *tpl.Spec.MaxReviewIterations != 2 {
		t.Errorf("maxReviewIterations = %v, want 2", tpl.Spec.MaxReviewIterations)
	}
	if tpl.Spec.MCPEnabled == nil || *tpl.Spec.MCPEnabled {
		t.Errorf("mcpEnabled = %v, want false", tpl.Spec.MCPEnabled)
	}
	if tpl.GPUGate == nil || tpl.GPUGate.Commands.Test != "cargo test -p dluth-cuda && bash tools/mutants.sh all" {
		t.Errorf("gpuGate not read: %+v", tpl.GPUGate)
	}
	if tpl.GPUNode != gpuNode {
		t.Errorf("gpuNode = %q, want %q", tpl.GPUNode, gpuNode)
	}
}

func TestLoadWorkloadTemplate_RejectsBadShapes(t *testing.T) {
	cases := map[string]string{
		"unknown key (a typo would silently drop the GPU gate)": "spec: {repo: a/b}\ngpugate: {language: generic}\n",
		"gpuGate without gpuNode":                               "spec: {repo: a/b}\ngpuGate: {language: generic}\n",
		"gpuNode without gpuGate":                               "spec: {repo: a/b}\ngpuNode: n1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "t.yaml")
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadWorkloadTemplate(p); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if _, err := LoadWorkloadTemplate(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing file: want an error")
	}
}

// --- Preflight -------------------------------------------------------------

// Review Focus 1: the loop restarts mid-run. The coder has already pushed the
// branch, which on its own would make preflight skip the item as done; the
// reserved Workload still being there means this run owns that branch, so
// preflight proceeds and Dispatch resumes it.
func TestPreflight_ExistingWorkloadResumes(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	existing := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS},
		Spec:       meithealv1alpha1.WorkloadSpec{Intent: "earlier run", Issues: []int32{effIssue}},
	}
	c := newEffectsClient(t, nil, existing)
	f := newFakeRunForge()
	branch := taskBranch(name, effIssue)
	f.branches[branch] = true
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())

	skip, err := e.Preflight(context.Background(), effItem("cpu"), branch)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if skip != "" {
		t.Fatalf("Preflight skipped a run whose Workload exists: %q", skip)
	}
}

func TestPreflight_NoWorkloadUsesForgeProbe(t *testing.T) {
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	branch := taskBranch(plannedWorkloadName(effIssue), effIssue)
	f.branches[branch] = true
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())

	skip, err := e.Preflight(context.Background(), effItem("cpu"), branch)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !strings.Contains(skip, "already exists") {
		t.Fatalf("skip = %q, want the branch-exists skip from the package Preflight", skip)
	}
}

// --- Dispatch --------------------------------------------------------------

func TestDispatch_CreatesWorkloadFromTemplate(t *testing.T) {
	c := newEffectsClient(t, nil)
	tpl := testTemplate()
	e := NewClusterEffects(c, effNS, newFakeRunForge(), tpl, &fakeGateRunner{}, fastOpts())

	got, err := e.Dispatch(context.Background(), effItem("cpu"), "fix the tokenizer")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got != plannedWorkloadName(effIssue) {
		t.Fatalf("Dispatch returned %q, want %q", got, plannedWorkloadName(effIssue))
	}
	w := getWorkload(t, c, got)
	want := tpl.Spec.DeepCopy()
	want.Intent = "fix the tokenizer"
	want.Issues = []int32{effIssue}
	want.Repo = effRepo
	want.ReviseExistingBranch = false
	if !reflect.DeepEqual(w.Spec, *want) {
		t.Fatalf("spec mismatch:\n got %+v\nwant %+v", w.Spec, *want)
	}
}

func TestDispatch_ItemRepoOverridesTemplate(t *testing.T) {
	c := newEffectsClient(t, nil)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	item := effItem("cpu")
	item.Repo = "defilantech/other"
	name, err := e.Dispatch(context.Background(), item, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got := getWorkload(t, c, name).Spec.Repo; got != "defilantech/other" {
		t.Fatalf("repo = %q, want the item's", got)
	}
}

// Review Focus 2: a Workload with the reserved name already exists (a crash,
// or a manual run). Dispatch resumes it: same name back, and no write of any
// kind against the cluster.
func TestDispatch_ExistingWorkloadResumes(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	existing := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS},
		Spec: meithealv1alpha1.WorkloadSpec{
			Intent: "a manual run's own intent",
			Repo:   effRepo,
			Issues: []int32{effIssue},
		},
	}
	var writes []string
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			writes = append(writes, "create "+o.GetName())
			return c.Create(ctx, o, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			writes = append(writes, "update "+o.GetName())
			return c.Update(ctx, o, opts...)
		},
		Patch: func(
			ctx context.Context, c client.WithWatch, o client.Object, p client.Patch, opts ...client.PatchOption,
		) error {
			writes = append(writes, "patch "+o.GetName())
			return c.Patch(ctx, o, p, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			writes = append(writes, "delete "+o.GetName())
			return c.Delete(ctx, o, opts...)
		},
	}
	c := newEffectsClient(t, funcs, existing)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())

	got, err := e.Dispatch(context.Background(), effItem("cpu"), "a different intent")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got != name {
		t.Fatalf("Dispatch returned %q, want the existing %q", got, name)
	}
	if len(writes) != 0 {
		t.Fatalf("Dispatch wrote to the cluster on resume: %v", writes)
	}
	if w := getWorkload(t, c, name); w.Spec.Intent != "a manual run's own intent" {
		t.Fatalf("existing Workload was changed: intent %q", w.Spec.Intent)
	}
}

// --- Redispatch ------------------------------------------------------------

func TestRedispatch_DeletesAndRecreatesWithRevise(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	existing := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS, UID: "first-attempt"},
		Spec:       meithealv1alpha1.WorkloadSpec{Intent: "fix it", Repo: effRepo, Issues: []int32{effIssue}},
		Status:     meithealv1alpha1.WorkloadStatus{Phase: meithealv1alpha1.WorkloadPhaseCompleted},
	}
	var order []string
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			order = append(order, "create")
			return c.Create(ctx, o, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			order = append(order, "delete")
			return c.Delete(ctx, o, opts...)
		},
	}
	c := newEffectsClient(t, funcs, existing)
	tpl := testTemplate()
	e := NewClusterEffects(c, effNS, newFakeRunForge(), tpl, &fakeGateRunner{}, fastOpts())

	head := strings.Repeat("H", 1000)
	tail := strings.Repeat("t", 3999) + "Z"
	if err := e.Redispatch(context.Background(), effItem("cpu"), name, "fix it", head+tail); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"delete", "create"}) {
		t.Fatalf("cluster writes = %v, want delete then create", order)
	}
	w := getWorkload(t, c, name)
	if w.UID == "first-attempt" || w.Status.Phase != "" {
		t.Fatalf("Workload was not recreated: uid %q phase %q", w.UID, w.Status.Phase)
	}
	if !w.Spec.ReviseExistingBranch {
		t.Fatal("recreated Workload must set ReviseExistingBranch")
	}
	wantIntent := "fix it" +
		"\n\nThe previous attempt failed the independent gate. Fix this and nothing else:\n" + tail
	if w.Spec.Intent != wantIntent {
		t.Fatalf("intent:\n got %q\nwant %q", w.Spec.Intent, wantIntent)
	}
	if !reflect.DeepEqual(w.Spec.Issues, []int32{effIssue}) || w.Spec.CoderAgentRef == nil ||
		w.Spec.CoderAgentRef.Name != tpl.Spec.CoderAgentRef.Name {
		t.Fatalf("template fields not carried over: %+v", w.Spec)
	}
}

func TestRedispatch_ShortEvidenceKeptWhole(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	existing := &meithealv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS},
		Spec: meithealv1alpha1.WorkloadSpec{Intent: "fix it"}}
	c := newEffectsClient(t, nil, existing)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	if err := e.Redispatch(context.Background(), effItem("cpu"), name, "fix it", "clippy: unused var"); err != nil {
		t.Fatal(err)
	}
	if got := getWorkload(t, c, name).Spec.Intent; !strings.HasSuffix(got, ":\nclippy: unused var") {
		t.Fatalf("intent = %q", got)
	}
}

// The delete is waited out: while the old object is still there (a
// finalizer, a slow GC) Redispatch must not try to create over it.
func TestRedispatch_WaitsForTheOldWorkloadToGo(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	existing := &meithealv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS},
		Spec: meithealv1alpha1.WorkloadSpec{Intent: "fix it"}}
	gets := 0
	var mu sync.Mutex
	funcs := &interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return nil // accepted, but the object lingers
		},
		Get: func(
			ctx context.Context, c client.WithWatch, k client.ObjectKey, o client.Object, opts ...client.GetOption,
		) error {
			mu.Lock()
			gets++
			n := gets
			mu.Unlock()
			if n == 3 {
				_ = c.Delete(ctx, &meithealv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS}})
			}
			return c.Get(ctx, k, o, opts...)
		},
	}
	c := newEffectsClient(t, funcs, existing)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	if err := e.Redispatch(context.Background(), effItem("cpu"), name, "fix it", "ev"); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}
	if gets < 3 {
		t.Fatalf("Redispatch polled %d times, want it to wait for the delete", gets)
	}
	if !getWorkload(t, c, name).Spec.ReviseExistingBranch {
		t.Fatal("not recreated")
	}
}

func TestRedispatch_DeleteWaitIsBoundedByContext(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	existing := &meithealv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS},
		Spec: meithealv1alpha1.WorkloadSpec{Intent: "fix it"}}
	funcs := &interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return nil },
	}
	c := newEffectsClient(t, funcs, existing)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.Redispatch(ctx, effItem("cpu"), name, "fix it", "ev"); err == nil {
		t.Fatal("want an error when the old Workload never goes away")
	}
}

// --- Watch -----------------------------------------------------------------

func coderTask(workload string, claimedAt *metav1.Time) *meithealv1alpha1.AgenticTask {
	step := fmt.Sprintf("code-%d", effIssue)
	return &meithealv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: workload + "-" + step, Namespace: effNS,
			Labels: map[string]string{"meitheal.dev/workload": workload, "meitheal.dev/step": step},
		},
		Spec: meithealv1alpha1.AgenticTaskSpec{Kind: meithealv1alpha1.AgenticTaskKindIssueFix},
		Status: meithealv1alpha1.AgenticTaskStatus{
			Phase:     meithealv1alpha1.AgenticTaskPhaseRunning,
			ClaimedAt: claimedAt,
		},
	}
}

func watchWorkload(name string, created time.Time, phase meithealv1alpha1.WorkloadPhase) *meithealv1alpha1.Workload {
	return &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS, CreationTimestamp: metav1.NewTime(created)},
		Spec:       meithealv1alpha1.WorkloadSpec{Intent: "x", Repo: effRepo, Issues: []int32{effIssue}},
		Status:     meithealv1alpha1.WorkloadStatus{Phase: phase},
	}
}

func setPhaseLater(
	t *testing.T, c client.Client, name string, phase meithealv1alpha1.WorkloadPhase, after time.Duration,
) {
	t.Helper()
	go func() {
		time.Sleep(after)
		var w meithealv1alpha1.Workload
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: effNS, Name: name}, &w); err != nil {
			return
		}
		w.Status.Phase = phase
		_ = c.Status().Update(context.Background(), &w)
	}()
}

// The stall clock starts when the coder claimed its task, not when the
// Workload was created: a Workload queued for 3h whose coder started 10m ago
// is not stalled against a 20m baseline.
func TestWatch_StallClockStartsAtClaim(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	claimed := metav1.NewTime(now.Add(-10 * time.Minute))
	c := newEffectsClient(t, nil,
		watchWorkload(name, now.Add(-3*time.Hour), meithealv1alpha1.WorkloadPhaseDispatched),
		coderTask(name, &claimed))
	o := fastOpts()
	o.Now = func() time.Time { return now }
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)
	// Completes after several polls, so a clock started at creation would
	// have reported a stall first.
	setPhaseLater(t, c, name, meithealv1alpha1.WorkloadPhaseCompleted, 30*time.Millisecond)

	res, err := e.Watch(context.Background(), name, 20*time.Minute, 2.5)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Stalled {
		t.Fatal("stalled, but the coder claimed only 10m ago against a 20m baseline")
	}
}

func TestWatch_StallsPastBaselineTimesFactorSinceClaim(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	claimed := metav1.NewTime(now.Add(-51 * time.Minute))
	c := newEffectsClient(t, nil,
		watchWorkload(name, now.Add(-52*time.Minute), meithealv1alpha1.WorkloadPhaseDispatched),
		coderTask(name, &claimed))
	o := fastOpts()
	o.Now = func() time.Time { return now }
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)

	res, err := e.Watch(context.Background(), name, 20*time.Minute, 2.5)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !res.Stalled {
		t.Fatal("not stalled 51m after claim against a 20m x 2.5 threshold")
	}
}

// An unclaimed coder never counts toward a stall, but Watch gives up after
// ClaimTimeout so a lane cannot hang forever on a task nobody picks up.
func TestWatch_UnclaimedGivesUpAfterClaimTimeout(t *testing.T) {
	start := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	c := newEffectsClient(t, nil,
		watchWorkload(name, start.Add(-30*time.Hour), meithealv1alpha1.WorkloadPhaseDispatched),
		coderTask(name, nil))
	var mu sync.Mutex
	calls := 0
	o := fastOpts()
	o.ClaimTimeout = 6 * time.Hour
	o.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return start.Add(time.Duration(calls) * time.Hour)
	}
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)

	res, err := e.Watch(context.Background(), name, 20*time.Minute, 2.5)
	if err == nil {
		t.Fatalf("want a claim-timeout error, got %+v", res)
	}
	if res.Stalled {
		t.Fatal("an unclaimed task must not be reported as stalled")
	}
	if !strings.Contains(err.Error(), "claim") {
		t.Fatalf("error should name the unclaimed task: %v", err)
	}
}

func TestWatch_ClaimTimeoutDefaultsToSixHours(t *testing.T) {
	start := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	c := newEffectsClient(t, nil,
		watchWorkload(name, start, meithealv1alpha1.WorkloadPhaseDispatched), coderTask(name, nil))
	var mu sync.Mutex
	var last time.Time
	calls := 0
	o := fastOpts()
	o.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		last = start.Add(time.Duration(calls) * time.Minute)
		return last
	}
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)
	if _, err := e.Watch(context.Background(), name, 0, 0); err == nil {
		t.Fatal("want a claim-timeout error")
	}
	if waited := last.Sub(start); waited < 6*time.Hour || waited > 6*time.Hour+10*time.Minute {
		t.Fatalf("gave up after %s, want about 6h", waited)
	}
}

func taskWithResult(
	workload, step string, kind meithealv1alpha1.AgenticTaskKind, result string,
) *meithealv1alpha1.AgenticTask {
	return &meithealv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: workload + "-" + step, Namespace: effNS,
			Labels: map[string]string{"meitheal.dev/workload": workload, "meitheal.dev/step": step},
		},
		Spec: meithealv1alpha1.AgenticTaskSpec{Kind: kind},
		Status: meithealv1alpha1.AgenticTaskStatus{
			Phase:  meithealv1alpha1.AgenticTaskPhaseSucceeded,
			Result: &runtime.RawExtension{Raw: []byte(result)},
		},
	}
}

func TestWatch_ReportsConcernAndFailure(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	name := plannedWorkloadName(effIssue)
	claimed := metav1.NewTime(now.Add(-5 * time.Minute))
	coder := coderTask(name, &claimed)
	coder.Status.Phase = meithealv1alpha1.AgenticTaskPhaseSucceeded
	coder.Status.Branch = taskBranch(name, effIssue)
	coder.Status.Result = &runtime.RawExtension{Raw: []byte(
		`{"verdict":"GO","extra":{"concerns":[{"kind":"novelty","text":"mirrors a kernel layout",` +
			`"files":["src/a.rs"]},{"kind":"novelty","text":"second"}]}}`)}
	review := taskWithResult(name, fmt.Sprintf("review-%d-0", effIssue), meithealv1alpha1.AgenticTaskKindReview,
		`{"verdict":"NO-GO","extra":{"pullRequestURL":"https://github.com/defilantech/dluth/pull/7"}}`)
	c := newEffectsClient(t, nil,
		watchWorkload(name, now.Add(-time.Hour), meithealv1alpha1.WorkloadPhaseFailed), coder, review)
	o := fastOpts()
	o.Now = func() time.Time { return now }
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, o)

	res, err := e.Watch(context.Background(), name, 20*time.Minute, 2.5)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if res.Concern != "mirrors a kernel layout" {
		t.Errorf("Concern = %q", res.Concern)
	}
	if !res.WorkloadFailed {
		t.Error("WorkloadFailed = false on a Failed Workload")
	}
	if !res.BranchPushed {
		t.Error("BranchPushed = false although the coder recorded its branch")
	}
	if res.PRURL != "https://github.com/defilantech/dluth/pull/7" {
		t.Errorf("PRURL = %q", res.PRURL)
	}
	if res.Stalled {
		t.Error("Stalled on a terminal Workload")
	}
}

func TestWatch_CompletedIsCleanAndMissingWorkloadErrors(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	c := newEffectsClient(t, nil, watchWorkload(name, time.Now(), meithealv1alpha1.WorkloadPhaseCompleted))
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	res, err := e.Watch(context.Background(), name, time.Hour, 2.5)
	if err != nil {
		t.Fatal(err)
	}
	if res != (WatchResult{}) {
		t.Fatalf("Completed Workload with no tasks: got %+v, want zero", res)
	}
	if _, err := e.Watch(context.Background(), "wl-missing", time.Hour, 2.5); err == nil {
		t.Fatal("watching a missing Workload must be an error")
	}
}

func TestWatch_HonorsContext(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	claimed := metav1.Now()
	c := newEffectsClient(t, nil,
		watchWorkload(name, time.Now(), meithealv1alpha1.WorkloadPhaseDispatched), coderTask(name, &claimed))
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.Watch(ctx, name, time.Hour, 2.5); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
}

// --- Kill ------------------------------------------------------------------

func TestKill_DeletesWorkloadAndToleratesMissing(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	c := newEffectsClient(t, nil, watchWorkload(name, time.Now(), meithealv1alpha1.WorkloadPhaseDispatched))
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	if err := e.Kill(context.Background(), name); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	var w meithealv1alpha1.Workload
	err := c.Get(context.Background(), types.NamespacedName{Namespace: effNS, Name: name}, &w)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Workload still there after Kill: %v", err)
	}
	if err := e.Kill(context.Background(), name); err != nil {
		t.Fatalf("second Kill: %v", err)
	}
}

// --- Verify ----------------------------------------------------------------

func dispatched(t *testing.T, e Effects, lane string) string {
	t.Helper()
	name, err := e.Dispatch(context.Background(), effItem(lane), "fix it")
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestVerify_GPULaneRunsBothGates(t *testing.T) {
	c := newEffectsClient(t, nil)
	g := &fakeGateRunner{}
	tpl := testTemplate()
	e := NewClusterEffects(c, effNS, newFakeRunForge(), tpl, g, fastOpts())
	name := dispatched(t, e, "gpu")
	branch := taskBranch(name, effIssue)

	clean, ev, err := e.Verify(context.Background(), name, branch)
	if err != nil || !clean {
		t.Fatalf("Verify = %v, %q, %v; want clean", clean, ev, err)
	}
	if len(g.calls) != 2 {
		t.Fatalf("gate runs = %d, want 2 (cpu then gpu)", len(g.calls))
	}
	if !reflect.DeepEqual(g.calls[0].profile, *tpl.Spec.GateProfile) {
		t.Errorf("first run profile = %+v, want the CPU gate", g.calls[0].profile)
	}
	if !reflect.DeepEqual(g.calls[1].profile, *tpl.GPUGate) {
		t.Errorf("second run profile = %+v, want the GPU gate", g.calls[1].profile)
	}
	for _, call := range g.calls {
		if call.repo != effRepo || call.branch != branch {
			t.Errorf("gate ran on %s@%s, want %s@%s", call.repo, call.branch, effRepo, branch)
		}
	}
}

func TestVerify_CPULaneRunsOnlyCPU(t *testing.T) {
	c := newEffectsClient(t, nil)
	g := &fakeGateRunner{}
	tpl := testTemplate()
	e := NewClusterEffects(c, effNS, newFakeRunForge(), tpl, g, fastOpts())
	name := dispatched(t, e, "cpu")

	clean, _, err := e.Verify(context.Background(), name, taskBranch(name, effIssue))
	if err != nil || !clean {
		t.Fatalf("Verify = %v, %v; want clean", clean, err)
	}
	if len(g.calls) != 1 || !reflect.DeepEqual(g.calls[0].profile, *tpl.Spec.GateProfile) {
		t.Fatalf("gate runs = %+v, want the CPU gate only", g.calls)
	}
}

func TestVerify_DirtyCPUGateSkipsGPUAndReturnsLog(t *testing.T) {
	c := newEffectsClient(t, nil)
	g := &fakeGateRunner{dirty: map[string]string{"rust:1": "error[E0308]: mismatched types"}}
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), g, fastOpts())
	name := dispatched(t, e, "gpu")
	clean, ev, err := e.Verify(context.Background(), name, taskBranch(name, effIssue))
	if err != nil || clean {
		t.Fatalf("Verify = %v, %v; want dirty", clean, err)
	}
	if !strings.Contains(ev, "error[E0308]: mismatched types") {
		t.Fatalf("evidence = %q, want the gate log", ev)
	}
	if len(g.calls) != 1 {
		t.Fatalf("gate runs = %d, want the GPU gate skipped after a dirty CPU gate", len(g.calls))
	}
}

func TestVerify_UnknownWorkloadIsAnError(t *testing.T) {
	c := newEffectsClient(t, nil)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	if _, _, err := e.Verify(context.Background(), "wl-9", "meitheal/wl-9/issue-9"); err == nil {
		t.Fatal("want an error for a Workload this run never dispatched")
	}
}

func inferencePod(name, node string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "inference", Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestVerify_GPUBusyRetriesThenParks(t *testing.T) {
	busy := inferencePod("qwen-0", gpuNode,
		map[string]string{LLMKubeInferenceServiceLabel: "qwen"}, corev1.PodRunning)
	lists := 0
	var mu sync.Mutex
	funcs := &interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if _, ok := l.(*corev1.PodList); ok {
				mu.Lock()
				lists++
				mu.Unlock()
			}
			return c.List(ctx, l, opts...)
		},
	}
	c := newEffectsClient(t, funcs, busy)
	g := &fakeGateRunner{}
	o := fastOpts()
	o.GPURetry = time.Millisecond
	o.GPUGiveUp = 5 * time.Millisecond
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), g, o)
	name := dispatched(t, e, "gpu")

	clean, ev, err := e.Verify(context.Background(), name, taskBranch(name, effIssue))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if clean || ev != "gpu-box-busy" {
		t.Fatalf("Verify = %v, %q; want not clean with evidence gpu-box-busy", clean, ev)
	}
	for _, call := range g.calls {
		if call.profile.Image == "dluth-cuda:1" {
			t.Fatal("the GPU gate ran on a busy node")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if lists < 2 {
		t.Fatalf("pod lists = %d, want the busy check retried", lists)
	}
}

func TestVerify_GPUFreesUpThenRuns(t *testing.T) {
	busy := inferencePod("qwen-0", gpuNode,
		map[string]string{LLMKubeInferenceServiceLabel: "qwen"}, corev1.PodRunning)
	c := newEffectsClient(t, nil, busy)
	g := &fakeGateRunner{}
	o := fastOpts()
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), g, o)
	name := dispatched(t, e, "gpu")
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = c.Delete(context.Background(), busy)
	}()
	clean, ev, err := e.Verify(context.Background(), name, taskBranch(name, effIssue))
	if err != nil || !clean {
		t.Fatalf("Verify = %v, %q, %v; want clean once the node frees up", clean, ev, err)
	}
	if len(g.calls) != 2 {
		t.Fatalf("gate runs = %d, want 2", len(g.calls))
	}
}

// --- Finalize --------------------------------------------------------------

func TestFinalize_UpdatesPRBodyAndComments(t *testing.T) {
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	name := dispatched(t, e, "gpu")
	branch := taskBranch(name, effIssue)
	url := "https://github.com/defilantech/dluth/pull/9"
	f.prs[branch] = url

	if err := e.Finalize(context.Background(), effItem("gpu"), name, branch); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	body := f.bodies[branch]
	for _, want := range []string{"## Summary", "## Testing", "#42",
		"Assisted-by: Meitheal; DeepSeek V4.1 Flash on a three-GB10 LLMKube ring wrote the change, " +
			"Qwen3.8 Flash-Next reviewed it; gates: " +
			"`cargo fmt --all -- --check`, `cargo clippy --all-targets -- -D warnings`, " +
			"`cargo build --release`, `cargo test && bash tools/mutants.sh cpu`, " +
			"`cargo test -p dluth-cuda && bash tools/mutants.sh all`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "## Summary") > strings.Index(body, "## Testing") {
		t.Error("Summary must come before Testing")
	}
	want := []forgeComment{{effRepo, effIssue, "Draft PR ready for review: " + url}}
	if !reflect.DeepEqual(f.comments, want) {
		t.Fatalf("comments = %+v, want %+v", f.comments, want)
	}
}

func TestFinalize_CPULaneListsOnlyCPUGatesAndKeepsCoderSummary(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	coder := taskWithResult(name, fmt.Sprintf("code-%d", effIssue), meithealv1alpha1.AgenticTaskKindIssueFix,
		`{"verdict":"GO","extra":{"modelExtra":{"prBody":"Reworks the tokenizer's merge loop."}}}`)
	c := newEffectsClient(t, nil, coder)
	f := newFakeRunForge()
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	name = dispatched(t, e, "cpu")
	branch := taskBranch(name, effIssue)
	f.prs[branch] = "https://github.com/defilantech/dluth/pull/9"
	if err := e.Finalize(context.Background(), effItem("cpu"), name, branch); err != nil {
		t.Fatal(err)
	}
	body := f.bodies[branch]
	if strings.Contains(body, "dluth-cuda") {
		t.Errorf("CPU-lane body lists the GPU gate:\n%s", body)
	}
	if !strings.Contains(body, "Reworks the tokenizer's merge loop.") {
		t.Errorf("coder's PR description dropped:\n%s", body)
	}
}

func TestFinalize_NoPRIsAnError(t *testing.T) {
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	name := dispatched(t, e, "cpu")
	if err := e.Finalize(context.Background(), effItem("cpu"), name, taskBranch(name, effIssue)); err == nil {
		t.Fatal("want an error when the run opened no PR")
	}
	if len(f.comments) != 0 {
		t.Fatalf("commented without a PR: %+v", f.comments)
	}
}

// --- Notify ----------------------------------------------------------------

type notifyPost struct {
	Issue    int32             `json:"issue"`
	Kind     string            `json:"kind"`
	Reason   string            `json:"reason"`
	Options  []string          `json:"options"`
	Evidence map[string]string `json:"evidence"`
}

// Review Focus 4: gate evidence never reaches the public issue, even when it
// carries a hostname and a token-shaped string.
func TestNotify_PublicCommentIsOneLine(t *testing.T) {
	var got notifyPost
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctype = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	o := fastOpts()
	o.NotifyURL = srv.URL
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, o)
	name := plannedWorkloadName(effIssue)
	branch := taskBranch(name, effIssue)
	url := "https://github.com/defilantech/dluth/pull/11"
	f.prs[branch] = url
	d := Decision{
		Issue: effIssue, Workload: name, Stage: "verify", Kind: ParkEscalate,
		Reason:  "verify failed after the feedback pass",
		Options: []string{"requeue", "decide", "hand-fix", "drop"},
		Evidence: map[string]string{
			"verify":  "ssh afi-spark-003: test failed\nGITHUB_TOKEN=ghp_x leaked",
			"concern": "copies afi-spark-003's layout",
		},
	}

	if err := e.Notify(context.Background(), effItem("gpu"), d); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	want := "Meitheal parked this issue: verify failed after the feedback pass. Draft PR: " + url
	if len(f.comments) != 1 || f.comments[0].body != want {
		t.Fatalf("comments = %+v, want exactly %q", f.comments, want)
	}
	for _, leak := range []string{"afi-spark-003", "ghp_x", "requeue", "\n"} {
		if strings.Contains(f.comments[0].body, leak) {
			t.Errorf("public comment contains %q", leak)
		}
	}
	if f.comments[0].issue != effIssue || f.comments[0].repo != effRepo {
		t.Errorf("comment went to %s#%d", f.comments[0].repo, f.comments[0].issue)
	}
	if !reflect.DeepEqual(f.labels[branch], []string{"meitheal:parked"}) {
		t.Errorf("PR labels = %v, want meitheal:parked", f.labels[branch])
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q", ctype)
	}
	wantPost := notifyPost{Issue: effIssue, Kind: d.Kind, Reason: d.Reason, Options: d.Options, Evidence: d.Evidence}
	if !reflect.DeepEqual(got, wantPost) {
		t.Errorf("notify POST = %+v, want %+v", got, wantPost)
	}
}

func TestNotify_NoPRMeansNoLinkAndNoLabel(t *testing.T) {
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	d := Decision{Issue: effIssue, Workload: plannedWorkloadName(effIssue), Kind: ParkEscalate, Reason: "stalled",
		Evidence: map[string]string{"verify": "ghp_x"}}
	if err := e.Notify(context.Background(), effItem("cpu"), d); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(f.comments) != 1 || f.comments[0].body != "Meitheal parked this issue: stalled." {
		t.Fatalf("comments = %+v", f.comments)
	}
	if len(f.labels) != 0 {
		t.Fatalf("labelled without a PR: %v", f.labels)
	}
}

// A reason is one line on the public issue no matter what it holds.
func TestNotify_ReasonIsFlattenedToOneLine(t *testing.T) {
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	d := Decision{Issue: effIssue, Kind: ParkNeedsDecision, Reason: "planner output\nunreadable."}
	if err := e.Notify(context.Background(), effItem("cpu"), d); err != nil {
		t.Fatal(err)
	}
	if got := f.comments[0].body; got != "Meitheal parked this issue: planner output unreadable." {
		t.Fatalf("comment = %q", got)
	}
}

func TestNotify_WebhookNon2xxIsAnErrorButCommentStillPosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	o := fastOpts()
	o.NotifyURL = srv.URL
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, o)
	d := Decision{Issue: effIssue, Kind: ParkEscalate, Reason: "stalled"}
	err := e.Notify(context.Background(), effItem("cpu"), d)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want the webhook's status", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comment not posted: %+v", f.comments)
	}
}

func TestNotify_PRLookupErrorStillCommentsWithoutLink(t *testing.T) {
	c := newEffectsClient(t, nil)
	f := newFakeRunForge()
	f.prErr = errors.New("api down")
	e := NewClusterEffects(c, effNS, f, testTemplate(), &fakeGateRunner{}, fastOpts())
	d := Decision{Issue: effIssue, Workload: plannedWorkloadName(effIssue), Kind: ParkEscalate, Reason: "stalled"}
	err := e.Notify(context.Background(), effItem("cpu"), d)
	if err == nil {
		t.Fatal("want the PR lookup error reported")
	}
	if len(f.comments) != 1 || f.comments[0].body != "Meitheal parked this issue: stalled." {
		t.Fatalf("comments = %+v", f.comments)
	}
}

// A Workload under the reserved name that is not running this issue is
// somebody else's: resuming it would end with Redispatch deleting it.
func TestDispatch_ExistingWorkloadForAnotherIssueIsAnError(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	other := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS},
		Spec:       meithealv1alpha1.WorkloadSpec{Intent: "manual", Issues: []int32{7}},
	}
	c := newEffectsClient(t, nil, other)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	if _, err := e.Dispatch(context.Background(), effItem("cpu"), "x"); err == nil {
		t.Fatal("want an error")
	}
	if got := getWorkload(t, c, name).Spec.Intent; got != "manual" {
		t.Fatalf("other run's Workload changed: intent %q", got)
	}
}

// A Workload still being deleted after a Kill is not resumed: Dispatch waits
// it out and creates a fresh one.
func TestDispatch_DeletingWorkloadIsWaitedOutThenCreated(t *testing.T) {
	name := plannedWorkloadName(effIssue)
	now := metav1.Now()
	going := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: effNS, DeletionTimestamp: &now,
			Finalizers: []string{"test.meitheal.dev/hold"}},
		Spec: meithealv1alpha1.WorkloadSpec{Intent: "killed run", Issues: []int32{effIssue}},
	}
	c := newEffectsClient(t, nil, going)
	e := NewClusterEffects(c, effNS, newFakeRunForge(), testTemplate(), &fakeGateRunner{}, fastOpts())
	go func() {
		time.Sleep(10 * time.Millisecond)
		w := getWorkload(t, c, name)
		w.Finalizers = nil
		_ = c.Update(context.Background(), w)
	}()
	got, err := e.Dispatch(context.Background(), effItem("cpu"), "fresh intent")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if w := getWorkload(t, c, got); w.Spec.Intent != "fresh intent" || w.DeletionTimestamp != nil {
		t.Fatalf("want a fresh Workload, got intent %q deleting=%v", w.Spec.Intent, w.DeletionTimestamp != nil)
	}
}
