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
	"errors"
	"fmt"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	sigsyaml "sigs.k8s.io/yaml"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

const (
	loopRepo     = planRepo
	loopAnswerer = "maintainer"
)

// --- fakes -----------------------------------------------------------------

// scriptEffects plays one scripted outcome per drive of an issue: "done"
// (a clean verify, then finalize), "park" (a stall), "skip" (preflight finds
// an open PR), "block" (watch waits for the context) or "error" (watch
// fails). The last outcome repeats; no script means "done". A drive starts at
// Preflight, or at Restart for a new round. Safe for the lanes' goroutines.
type scriptEffects struct {
	mu        sync.Mutex
	outcomes  map[int32][]string
	drives    map[int32]int
	current   map[string]string // workload -> outcome of this drive
	order     []int32
	restarted []int32
	intents   map[int32][]string
	notified  []Decision
	kills     int
	blocked   chan int32
}

func newScriptEffects() *scriptEffects {
	return &scriptEffects{
		drives: map[int32]int{}, current: map[string]string{}, intents: map[int32][]string{},
		blocked: make(chan int32, 16),
	}
}

func (s *scriptEffects) preflighted() []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.order...)
}

func (s *scriptEffects) notifications() []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Decision(nil), s.notified...)
}

func (s *scriptEffects) restarts() []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.restarted...)
}

func (s *scriptEffects) outcome(issue int32) string {
	if o := s.outcomes[issue]; len(o) > 0 {
		return o[min(s.drives[issue]-1, len(o)-1)]
	}
	return "done"
}

func (s *scriptEffects) Preflight(_ context.Context, item QueueItem, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, item.Issue)
	s.drives[item.Issue]++
	if s.outcome(item.Issue) == "skip" {
		return "an open PR already references it: https://example.test/pr/other", nil
	}
	return "", nil
}

func (s *scriptEffects) Restart(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	s.mu.Lock()
	s.restarted = append(s.restarted, item.Issue)
	s.drives[item.Issue]++
	s.mu.Unlock()
	return s.Dispatch(ctx, item, intent)
}

func (s *scriptEffects) Dispatch(_ context.Context, item QueueItem, intent string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := plannedWorkloadName(item.Issue)
	s.current[w] = s.outcome(item.Issue)
	s.intents[item.Issue] = append(s.intents[item.Issue], intent)
	return w, 1, nil
}

func (s *scriptEffects) Watch(ctx context.Context, workload string, _ time.Duration, _ float64) (WatchResult, error) {
	s.mu.Lock()
	out := s.current[workload]
	s.mu.Unlock()
	switch out {
	case "park":
		return WatchResult{Stalled: true}, nil
	case "block":
		var n int32
		_, _ = fmt.Sscanf(workload, "wl-%d", &n)
		s.blocked <- n
		<-ctx.Done()
		return WatchResult{}, ctx.Err()
	case "error":
		return WatchResult{}, errors.New("watch failed")
	}
	return WatchResult{PRURL: "https://example.test/pr/" + strings.TrimPrefix(workload, "wl-")}, nil
}

func (s *scriptEffects) Kill(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kills++
	return nil
}

func (s *scriptEffects) Verify(context.Context, string, string) (bool, string, error) {
	return true, "", nil
}

func (s *scriptEffects) Redispatch(context.Context, QueueItem, string, string, string) error {
	return nil
}

func (s *scriptEffects) Finalize(context.Context, QueueItem, string, string) error { return nil }

func (s *scriptEffects) Notify(_ context.Context, _ QueueItem, d Decision) (ParkNotices, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notified = append(s.notified, d)
	return ParkNotices{}, nil
}

// loopForge serves issues and comments, honours since, and records what the
// loop posts.
type loopForge struct {
	mu       sync.Mutex
	issues   map[int32]IssueDetail
	comments map[int32][]IssueComment
	posted   []forgeComment
}

func newLoopForge() *loopForge {
	return &loopForge{issues: map[int32]IssueDetail{}, comments: map[int32][]IssueComment{}}
}

func (f *loopForge) answer(issue int32, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[issue] = append(f.comments[issue], IssueComment{Author: loopAnswerer, Body: body, Created: at})
}

func (f *loopForge) postedTo(issue int32) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.posted {
		if c.issue == issue {
			out = append(out, c.body)
		}
	}
	return out
}

func (f *loopForge) OpenPRForIssue(context.Context, string, int32) (string, error) { return "", nil }
func (f *loopForge) BranchExists(context.Context, string, string) (bool, error)    { return false, nil }
func (f *loopForge) PRForBranch(context.Context, string, string) (string, error)   { return "", nil }
func (f *loopForge) UpdatePRBody(context.Context, string, string, string) error    { return nil }
func (f *loopForge) LabelPR(context.Context, string, string, string) error         { return nil }

func (f *loopForge) CommentIssue(_ context.Context, repo string, issue int32, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if repo != loopRepo {
		return fmt.Errorf("unexpected repo %q", repo)
	}
	f.posted = append(f.posted, forgeComment{repo, issue, body})
	f.comments[issue] = append(f.comments[issue], IssueComment{Author: "meitheal-bot", Body: body, Created: time.Now()})
	return nil
}

func (f *loopForge) CommentsSince(
	_ context.Context, repo string, issue int32, since time.Time,
) ([]IssueComment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if repo != loopRepo {
		return nil, fmt.Errorf("unexpected repo %q", repo)
	}
	var out []IssueComment
	for _, c := range f.comments[issue] {
		if !c.Created.Before(since) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *loopForge) FetchIssue(_ context.Context, repo string, issue int32) (IssueDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if repo != loopRepo {
		return IssueDetail{}, fmt.Errorf("unexpected repo %q", repo)
	}
	d, ok := f.issues[issue]
	if !ok {
		return IssueDetail{}, fmt.Errorf("no issue %d", issue)
	}
	return d, nil
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// --- environment -----------------------------------------------------------

type loopEnv struct {
	root    string
	cfg     runLoopConfig
	effects *scriptEffects
	forge   *loopForge
	logs    *logSink
}

func newLoopEnv(t *testing.T) *loopEnv {
	t.Helper()
	root := t.TempDir()
	e := &loopEnv{root: root, effects: newScriptEffects(), forge: newLoopForge(), logs: &logSink{}}
	e.cfg = runLoopConfig{
		Effects:      e.effects,
		Forge:        e.forge,
		DecisionsDir: filepath.Join(root, "decisions"),
		LedgerPath:   filepath.Join(root, "ledger.jsonl"),
		Answerer:     loopAnswerer,
		Repo:         loopRepo,
		Baseline:     time.Hour,
		Factor:       DefaultStallFactor,
		Limits:       map[string]int{"cpu": 1, "gpu": 1},
		Once:         true,
		Logf:         e.logs.logf,
	}
	return e
}

// queue writes an intent file per item and fills in repo and lane.
func (e *loopEnv) queue(t *testing.T, items ...QueueItem) []QueueItem {
	t.Helper()
	dir := filepath.Join(e.root, "intents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range items {
		p := filepath.Join(dir, fmt.Sprintf("%d.md", items[i].Issue))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("intent for %d", items[i].Issue)), 0o600); err != nil {
			t.Fatal(err)
		}
		items[i].IntentPath = p
		if items[i].Repo == "" {
			items[i].Repo = loopRepo
		}
		if items[i].Lane == "" {
			items[i].Lane = "cpu"
		}
	}
	return items
}

// park writes a decision that was opened an hour ago, stamped as notified
// when notified is set (a park an earlier run already announced).
func (e *loopEnv) park(t *testing.T, issue int32, kind, stage string, notified bool) time.Time {
	t.Helper()
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if _, err := ParkDecision(e.cfg.DecisionsDir, Decision{
		Repo: e.cfg.Repo, Issue: issue, Workload: plannedWorkloadName(issue), Stage: stage, Kind: kind, Opened: opened,
		Reason: "stalled", Options: optionsFor(kind),
	}); err != nil {
		t.Fatal(err)
	}
	if notified {
		if err := RecordNotices(e.cfg.DecisionsDir, issue, kind, ParkNotices{}, true, opened.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	return opened
}

func (e *loopEnv) run(t *testing.T, items []QueueItem) {
	t.Helper()
	if err := e.runErr(t, context.Background(), items); err != nil {
		t.Fatalf("run: %v\nlog:\n%s", err, e.logs)
	}
}

func (e *loopEnv) runErr(t *testing.T, ctx context.Context, items []QueueItem) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- runQueueLoop(ctx, e.cfg, items) }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatalf("run did not return\nlog:\n%s", e.logs)
		return nil
	}
}

func (e *loopEnv) decision(t *testing.T, issue int32, kind string) (Decision, bool) {
	t.Helper()
	ds, err := ListDecisions(e.cfg.DecisionsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.Issue == issue && d.Kind == kind {
			return d, true
		}
	}
	return Decision{}, false
}

func (e *loopEnv) ledger(t *testing.T) map[int32][]LedgerRecord {
	t.Helper()
	out := map[int32][]LedgerRecord{}
	for _, l := range ledgerLines(t, e.cfg.LedgerPath) {
		n := int32(l["issue"].(float64))
		out[n] = append(out[n], LedgerRecord{
			Issue: n, FinalStage: l["finalStage"].(string), ParkKind: l["parkKind"].(string),
			Reason: l["reason"].(string), Workload: l["workload"].(string),
		})
	}
	return out
}

// neutralTemplate is a one-gate template with no GPU lane.
func neutralTemplate() WorkloadTemplate {
	return WorkloadTemplate{Spec: meithealv1alpha1.WorkloadSpec{
		Intent:        "template placeholder",
		Repo:          loopRepo,
		CoderAgentRef: &corev1.LocalObjectReference{Name: "example-coder"},
		GateProfile: &meithealv1alpha1.GateProfile{
			Language: meithealv1alpha1.GateLanguageGeneric,
			Image:    "example/gate:1",
			Commands: meithealv1alpha1.GateCommands{Test: "make test"},
		},
	}}
}

// --- tests -----------------------------------------------------------------

// Review Focus 1: the loop restarts mid-run. 52 has a parked decision file
// and stays parked; 56's Workload is already in the cluster, so 56 resumes at
// watch against the real cluster Effects instead of dispatching a duplicate.
func TestRun_RestartResumes(t *testing.T) {
	env := newLoopEnv(t)
	env.park(t, 52, ParkEscalate, string(StageWatch), true)

	existing := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name: plannedWorkloadName(56), Namespace: effNS,
			Annotations: map[string]string{runAttemptAnnotation: "1"},
		},
		Spec:   meithealv1alpha1.WorkloadSpec{Intent: "the first run's intent", Repo: loopRepo, Issues: []int32{56}},
		Status: meithealv1alpha1.WorkloadStatus{Phase: meithealv1alpha1.WorkloadPhaseCompleted},
	}
	var mu sync.Mutex
	var writes []string
	note := func(s string) { mu.Lock(); defer mu.Unlock(); writes = append(writes, s) }
	funcs := &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			note("create " + o.GetName())
			return c.Create(ctx, o, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			note("update " + o.GetName())
			return c.Update(ctx, o, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
			note("delete " + o.GetName())
			return c.Delete(ctx, o, opts...)
		},
	}
	c := newEffectsClient(t, funcs, existing)
	forge := newFakeRunForge()
	branch56 := taskBranch(plannedWorkloadName(56), 56)
	forge.prs[branch56] = "https://example.test/pr/56"
	gate := &fakeGateRunner{}
	env.cfg.Effects = NewClusterEffects(c, effNS, forge, neutralTemplate(), gate, fastOpts())
	env.cfg.Forge = forge
	env.cfg.Answerer = ""

	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 56})
	env.run(t, items)

	if len(writes) != 0 {
		t.Fatalf("cluster writes = %v, want none: 52 is parked and 56 resumes its Workload", writes)
	}
	if w := getWorkload(t, c, plannedWorkloadName(56)); w.Spec.Intent != "the first run's intent" {
		t.Fatalf("56's Workload was replaced: intent %q", w.Spec.Intent)
	}
	if len(gate.calls) != 1 || gate.calls[0].branch != branch56 {
		t.Fatalf("gate calls = %+v, want one verify of %s", gate.calls, branch56)
	}
	if _, ok := env.decision(t, 52, ParkEscalate); !ok {
		t.Fatal("52's parked decision is gone; it must stay parked")
	}
	led := env.ledger(t)
	if len(led) != 1 || len(led[56]) != 1 || led[56][0].FinalStage != ledgerDone || led[56][0].Workload != "wl-56" {
		t.Fatalf("ledger = %+v, want one done record for 56 on wl-56", led)
	}
	for _, cm := range forge.comments {
		if cm.issue == 52 {
			t.Fatalf("52 got a comment %q; its park was announced by the earlier run", cm.body)
		}
	}
}

func TestRun_AnswerRequeueRedrives(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 52, ParkEscalate, string(StageWatch), true)
	env.forge.comments[52] = []IssueComment{
		{Author: "someone-else", Body: "/meitheal drop", Created: opened.Add(time.Minute)},
	}
	env.forge.answer(52, "/meitheal requeue", opened.Add(2*time.Minute))
	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53, Lane: "gpu", After: []int32{52}})

	env.run(t, items)

	if got := env.effects.restarts(); !reflect.DeepEqual(got, []int32{52}) {
		t.Fatalf("restarted = %v, want 52 in a new round", got)
	}
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{53}) {
		t.Fatalf("preflighted = %v, want only 53 (which runs after 52): a new round skips preflight", got)
	}
	if _, ok := env.decision(t, 52, ParkEscalate); ok {
		t.Fatal("the requeued decision file is still there")
	}
	if got := env.effects.intents[52]; !reflect.DeepEqual(got, []string{"intent for 52"}) {
		t.Fatalf("dispatched intents = %v", got)
	}
	led := env.ledger(t)
	if led[52][0].FinalStage != ledgerDone || led[53][0].FinalStage != ledgerDone {
		t.Fatalf("ledger = %+v, want 52 and 53 done", led)
	}
	if got := env.forge.postedTo(52); len(got) != 0 {
		t.Fatalf("posted to 52: %v; a non-answerer's drop must not count", got)
	}
}

// replanEnv wires the real planner path with a stub model.
func replanEnv(t *testing.T, env *loopEnv, answer string) *stubPlanner {
	t.Helper()
	env.forge.issues[53] = IssueDetail{Title: "Add a widget parser", Body: "The parser goes in src/lib.rs."}
	stub := &stubPlanner{answers: map[int32]string{53: answer}}
	env.cfg.Replan = newReplanner(env.forge, stub.call, loopRepo, gitRepo(t, defaultRepoFiles()),
		filepath.Join(env.root, "out"), env.cfg.DecisionsDir, PlanConfig{DecisionLabel: "decision"})
	env.cfg.QueueFile = filepath.Join(env.root, "out", queueFileName)
	return stub
}

func TestRun_AnswerDecideReplans(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 53, ParkNeedsDecision, intentsStage, true)
	env.forge.answer(53, "thanks\n/meitheal decide use the existing tokenizer, not a new one", opened.Add(time.Minute))
	stub := replanEnv(t, env, plannedAnswer("cpu", "src/lib.rs"))

	env.run(t, nil)

	prompt := stub.prompts[53]
	if !strings.Contains(prompt, maintainerDecisionHeading) ||
		!strings.Contains(prompt, "use the existing tokenizer, not a new one") {
		t.Fatalf("planner prompt lacks the maintainer decision:\n%s", prompt)
	}
	if _, ok := env.decision(t, 53, ParkNeedsDecision); ok {
		t.Fatal("the old decision file is still there after a successful re-plan")
	}
	if left, _ := filepath.Glob(filepath.Join(env.cfg.DecisionsDir, "*"+replanSuffix)); len(left) != 0 {
		t.Fatalf("re-plan left %v behind", left)
	}
	want := []string{"## Intent\n\nChange the thing. Mirror `src/lib.rs`."}
	if got := env.effects.intents[53]; !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatched intents = %q, want the re-planned intent %q", got, want)
	}
	if led := env.ledger(t); len(led[53]) != 1 || led[53][0].FinalStage != ledgerDone {
		t.Fatalf("ledger = %+v, want 53 done", led)
	}
	// Persisted, so a restart knows the item and its new intent.
	b, err := os.ReadFile(env.cfg.QueueFile)
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParseQueue(b)
	if err != nil || len(q.Items) != 1 || q.Items[0].Issue != 53 ||
		q.Items[0].IntentPath != filepath.Join(env.root, "out", "53.intent.md") {
		t.Fatalf("persisted queue = %+v, %v", q, err)
	}
}

// A re-plan that parks again, under the same kind, writes a fresh park that
// is announced; the answered one does not shadow it.
func TestRun_AnswerDecideParkedAgainNotifies(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 53, ParkNeedsDecision, intentsStage, true)
	env.forge.answer(53, "/meitheal decide keep the old format", opened.Add(time.Minute))
	replanEnv(t, env, "VERDICT: needs-decision\nreason: still open\n")

	env.run(t, nil)

	d, ok := env.decision(t, 53, ParkNeedsDecision)
	if !ok {
		t.Fatal("no new park for 53")
	}
	if d.Answer != "" || !d.Opened.After(opened) || d.Notified == nil {
		t.Fatalf("decision = %+v, want a fresh, unanswered, notified park", d)
	}
	if n := env.effects.notifications(); len(n) != 1 || n[0].Issue != 53 {
		t.Fatalf("notified = %+v, want the new park announced once", n)
	}
	if got := env.effects.preflighted(); len(got) != 0 {
		t.Fatalf("driven = %v, want nothing: the issue parked again", got)
	}
}

func TestRun_AnswerDecideWithoutAPlannerWaits(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 53, ParkNeedsDecision, intentsStage, true)
	env.forge.answer(53, "/meitheal decide keep the old format", opened.Add(time.Minute))
	l := newRunLoop(env.cfg)
	l.answerCycle(context.Background())
	l.answerCycle(context.Background())
	d, ok := env.decision(t, 53, ParkNeedsDecision)
	if !ok || d.Answer != "decide: keep the old format" {
		t.Fatalf("decision = %+v, %v; want it kept, answered", d, ok)
	}
	if n := env.logs.count("needs the planner"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, env.logs)
	}
}

func TestRun_AnswerDropFinishes(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 54, ParkEscalate, string(StageVerify), true)
	env.forge.answer(54, "/meitheal drop", opened.Add(time.Minute))
	items := env.queue(t, QueueItem{Issue: 54}, QueueItem{Issue: 55, After: []int32{54}})

	env.run(t, items)
	// A restart must not comment, record or drive again.
	env.run(t, items)

	if got := env.forge.postedTo(54); !reflect.DeepEqual(got, []string{dropComment}) {
		t.Fatalf("posted to 54 = %q, want the drop comment exactly once", got)
	}
	led := env.ledger(t)
	if len(led[54]) != 1 || led[54][0].FinalStage != ledgerDropped || led[54][0].ParkKind != ParkEscalate {
		t.Fatalf("ledger for 54 = %+v, want one dropped record", led[54])
	}
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{55}) {
		t.Fatalf("driven = %v, want only 55, once, released by the drop", got)
	}
	// Nothing else is removed: the decision stays as the record of the answer.
	if d, ok := env.decision(t, 54, ParkEscalate); !ok || d.Answer != "drop" {
		t.Fatalf("decision = %+v, %v; want it kept with its answer", d, ok)
	}
}

func TestRun_AnswerHandFixFinishesWithoutAComment(t *testing.T) {
	env := newLoopEnv(t)
	env.park(t, 54, ParkEscalate, string(StageVerify), true)
	if err := AnswerDecision(env.cfg.DecisionsDir, 54, ParkEscalate, "hand-fix"); err != nil {
		t.Fatal(err)
	}
	items := env.queue(t, QueueItem{Issue: 54})
	env.run(t, items)
	if got := env.forge.postedTo(54); len(got) != 0 {
		t.Fatalf("posted %q, want no comment for hand-fix", got)
	}
	if led := env.ledger(t); len(led[54]) != 1 || led[54][0].FinalStage != ledgerDone {
		t.Fatalf("ledger = %+v, want 54 done", led)
	}
	if got := env.effects.preflighted(); len(got) != 0 {
		t.Fatalf("driven = %v, want nothing", got)
	}
}

// Decision 3: every park reaches Notify once, the planner's included, and a
// stamped park is never announced again.
func TestRun_EveryParkIsNotifiedOnce(t *testing.T) {
	env := newLoopEnv(t)
	env.park(t, 60, ParkNeedsSplit, intentsStage, false)
	env.effects.outcomes = map[int32][]string{52: {"park"}}
	items := env.queue(t, QueueItem{Issue: 52})

	env.run(t, items)
	env.run(t, items)

	got := map[int32]int{}
	for _, d := range env.effects.notifications() {
		got[d.Issue]++
	}
	if !reflect.DeepEqual(got, map[int32]int{52: 1, 60: 1}) {
		t.Fatalf("notifications per issue = %v, want one each for 52 and 60", got)
	}
	for _, k := range []struct {
		issue int32
		kind  string
	}{{52, ParkEscalate}, {60, ParkNeedsSplit}} {
		if d, ok := env.decision(t, k.issue, k.kind); !ok || d.Notified == nil {
			t.Fatalf("decision %d/%s = %+v, want it stamped notified", k.issue, k.kind, d)
		}
	}
}

func TestDecision_FileWithoutNotifiedParses(t *testing.T) {
	env := newLoopEnv(t)
	if err := os.MkdirAll(env.cfg.DecisionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "issue: 61\nkind: needs-decision\nstage: plan\nopened: \"2026-11-01T10:00:00Z\"\nreason: x\n" +
		"options: [requeue, decide, drop]\n"
	if err := os.WriteFile(filepath.Join(env.cfg.DecisionsDir, "61-needs-decision.yaml"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	d, ok := env.decision(t, 61, ParkNeedsDecision)
	if !ok || d.Notified != nil {
		t.Fatalf("old decision = %+v, %v; want it parsed with no notified stamp", d, ok)
	}
	// A park from before decisions named their repository is this run's
	// only when its issue is in the queue.
	env.run(t, env.queue(t, QueueItem{Issue: 61}))
	if n := env.effects.notifications(); len(n) != 1 || n[0].Issue != 61 {
		t.Fatalf("notified = %+v, want the old park announced once", n)
	}
	if got := env.effects.preflighted(); len(got) != 0 {
		t.Fatalf("driven = %v, want 61 kept parked by its old decision", got)
	}
}

func TestRun_RejectedAnswerIsLoggedOnce(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 7, ParkAdjudicate, string(StageVerify), true)
	env.forge.answer(7, "/meitheal requeue", opened.Add(time.Minute))
	l := newRunLoop(env.cfg)
	for range 3 {
		l.answerCycle(context.Background())
	}
	if n := env.logs.count("issue 7"); n != 1 {
		t.Fatalf("rejection logged %d times, want once:\n%s", n, env.logs)
	}
}

// Decision 6: a dependent of an item that parks during the run waits, and a
// --once run still ends.
func TestRun_DependentOfAParkWaitsAndTheRunEnds(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"park"}}
	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53, After: []int32{52}})
	env.run(t, items)
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{52}) {
		t.Fatalf("driven = %v, want only 52", got)
	}
}

func TestRun_PollingPicksUpALaterAnswer(t *testing.T) {
	env := newLoopEnv(t)
	env.cfg.Once = false
	env.cfg.PollEvery = 5 * time.Millisecond
	env.effects.outcomes = map[int32][]string{52: {"park", "done"}}
	items := env.queue(t, QueueItem{Issue: 52})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runQueueLoop(ctx, env.cfg, items) }()

	waitFor(t, func() bool { _, ok := env.decision(t, 52, ParkEscalate); return ok })
	env.forge.answer(52, "/meitheal requeue", time.Now().Add(time.Second))
	waitFor(t, func() bool {
		_, err := os.Stat(env.cfg.LedgerPath)
		return err == nil && len(env.ledger(t)[52]) == 2
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if led := env.ledger(t)[52]; led[0].FinalStage != ledgerParked || led[1].FinalStage != ledgerDone {
		t.Fatalf("ledger for 52 = %+v, want parked then done", led)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Decision 8: a signal stops in-flight items at their next poll, without a
// park, a kill or a ledger record, so a restart resumes them.
func TestRun_CancelStopsWithoutParking(t *testing.T) {
	env := newLoopEnv(t)
	env.cfg.Once = false
	env.cfg.PollEvery = time.Hour
	env.effects.outcomes = map[int32][]string{52: {"block"}}
	items := env.queue(t, QueueItem{Issue: 52})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runQueueLoop(ctx, env.cfg, items) }()
	select {
	case <-env.effects.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("52 never reached watch")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v, want a clean stop", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop on cancel")
	}
	if ds, _ := ListDecisions(env.cfg.DecisionsDir); len(ds) != 0 {
		t.Fatalf("decisions = %+v, want no park on a stop", ds)
	}
	if env.effects.kills != 0 {
		t.Fatalf("kills = %d, want the Workload left alone", env.effects.kills)
	}
	if _, err := os.Stat(env.cfg.LedgerPath); !os.IsNotExist(err) {
		t.Fatalf("ledger written on a stop (stat err %v)", err)
	}
}

func TestRun_RefusesAQueueItCannotSchedule(t *testing.T) {
	env := newLoopEnv(t)
	cases := map[string][]QueueItem{
		"duplicate": env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 52}),
		"cycle":     env.queue(t, QueueItem{Issue: 52, After: []int32{53}}, QueueItem{Issue: 53, After: []int32{52}}),
		"two repos": env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53, Repo: "example/other"}),
	}
	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			if err := env.runErr(t, context.Background(), items); err == nil {
				t.Fatal("run = nil, want the queue refused")
			}
			if got := env.effects.preflighted(); len(got) != 0 {
				t.Fatalf("driven = %v before the queue was refused", got)
			}
		})
	}
}

func TestRun_EmptyQueueSaysSo(t *testing.T) {
	env := newLoopEnv(t)
	env.run(t, nil)
	if env.logs.count("queue empty") != 1 {
		t.Fatalf("log = %s, want it to say the queue is empty", env.logs)
	}
}

// A restarted --plan keeps the intents it already wrote and does not plan a
// parked issue again (that would refresh the park and lose its answers).
func TestPlanForRun_RestartKeepsQueueAndSkipsParked(t *testing.T) {
	env := newLoopEnv(t)
	out := filepath.Join(env.root, "out")
	kept := env.queue(t, QueueItem{Issue: 52})
	if err := writeQueueFile(filepath.Join(out, queueFileName), Queue{Repo: loopRepo, Items: kept}); err != nil {
		t.Fatal(err)
	}
	env.park(t, 53, ParkNeedsDecision, intentsStage, true)
	if err := NewLedger(env.cfg.LedgerPath).Append(LedgerRecord{Issue: 54, FinalStage: ledgerDone}); err != nil {
		t.Fatal(err)
	}
	env.forge.issues[55] = IssueDetail{Title: "Add a widget", Body: "In src/lib.rs."}
	stub := &stubPlanner{answers: map[int32]string{55: plannedAnswer("gpu", "src/lib.rs")}}
	opts := &runOptions{decisionsDir: env.cfg.DecisionsDir, ledgerPath: env.cfg.LedgerPath}
	opts.planner.out = out
	opts.planner.repoDir = gitRepo(t, defaultRepoFiles())

	var w strings.Builder
	items, err := planForRun(context.Background(), &w, env.forge, stub.call, loopRepo,
		[]int32{52, 53, 54, 55}, opts, PlanConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("planner called %d times, want once (for 55 only)", stub.calls)
	}
	got := make([]int32, 0, len(items))
	for _, it := range items {
		got = append(got, it.Issue)
		if it.Repo != loopRepo {
			t.Errorf("issue %d has repo %q", it.Issue, it.Repo)
		}
	}
	if !reflect.DeepEqual(got, []int32{52, 55}) {
		t.Fatalf("items = %v, want the kept 52 and the newly planned 55", got)
	}
	if items[0].IntentPath != kept[0].IntentPath {
		t.Fatalf("52's intent moved to %s", items[0].IntentPath)
	}
	b, err := os.ReadFile(filepath.Join(out, queueFileName))
	if err != nil {
		t.Fatal(err)
	}
	if q, err := ParseQueue(b); err != nil || len(q.Items) != 2 {
		t.Fatalf("written queue = %+v, %v; want both items", q, err)
	}
	if _, ok := env.decision(t, 53, ParkNeedsDecision); !ok {
		t.Fatal("53's park is gone")
	}
}

func TestSetCoderAgent(t *testing.T) {
	tpl := neutralTemplate()
	if err := setCoderAgent(&tpl, "example-coder"); err != nil {
		t.Fatalf("same agent: %v", err)
	}
	if err := setCoderAgent(&tpl, "other-coder"); err == nil {
		t.Fatal("a template naming another coder must be refused")
	}
	tpl.Spec.CoderAgentRef = nil
	if err := setCoderAgent(&tpl, "other-coder"); err != nil || tpl.Spec.CoderAgentRef.Name != "other-coder" {
		t.Fatalf("empty template coder: %v, %+v", err, tpl.Spec.CoderAgentRef)
	}
}

// The template in docs/run-loop.md is one LoadWorkloadTemplate accepts, so
// the page cannot drift into a file the run refuses.
func TestRunLoopDoc_TemplateLoads(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "run-loop.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "## The template")
	if !ok {
		t.Fatal("no template section")
	}
	_, rest, ok = strings.Cut(rest, "```yaml\n")
	block, _, closed := strings.Cut(rest, "```")
	if !ok || !closed {
		t.Fatal("no yaml block in the template section")
	}
	p := filepath.Join(t.TempDir(), "template.yaml")
	if err := os.WriteFile(p, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	tpl, err := LoadWorkloadTemplate(p)
	if err != nil {
		t.Fatalf("the documented template does not load: %v", err)
	}
	if tpl.GPUGate == nil || tpl.GPUNode == "" || len(tpl.Spec.ReviewerAgentRefs) != 1 || tpl.AssistedBy == "" {
		t.Fatalf("template = %+v", tpl)
	}
}

// roundEnv is a parked escalate for 53 whose round is spent: an attempt-2
// Workload that failed, a pushed branch and an open draft PR. Its Effects
// are the real cluster ones.
type roundEnv struct {
	*loopEnv
	c      client.WithWatch
	forge  *fakeRunForge
	gate   *fakeGateRunner
	branch string
}

func newRoundEnv(t *testing.T) *roundEnv {
	t.Helper()
	env := newLoopEnv(t)
	name := plannedWorkloadName(53)
	old := &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: effNS, Annotations: map[string]string{runAttemptAnnotation: "2"},
		},
		Spec: meithealv1alpha1.WorkloadSpec{
			Intent: "the feedback pass", Repo: loopRepo, Issues: []int32{53}, ReviseExistingBranch: true,
		},
		Status: meithealv1alpha1.WorkloadStatus{Phase: meithealv1alpha1.WorkloadPhaseFailed},
	}
	c := newEffectsClient(t, nil, old)
	forge := newFakeRunForge()
	branch := taskBranch(name, 53)
	forge.branches[branch] = true
	forge.prs[branch] = "https://example.test/pr/53"
	// Preflight would skip on this: the draft PR is the loop's own.
	forge.openPR = "https://example.test/pr/53"
	gate := &fakeGateRunner{}
	env.cfg.Effects = NewClusterEffects(c, effNS, forge, neutralTemplate(), gate, fastOpts())
	return &roundEnv{loopEnv: env, c: c, forge: forge, gate: gate, branch: branch}
}

// completeNewRound marks the recreated Workload Completed once it exists, as
// the controller would when its pipeline finishes.
func (r *roundEnv) completeNewRound(t *testing.T) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var w meithealv1alpha1.Workload
			err := r.c.Get(context.Background(), client.ObjectKey{Namespace: effNS, Name: plannedWorkloadName(53)}, &w)
			if err == nil && w.Annotations[runAttemptAnnotation] == "1" && w.DeletionTimestamp == nil {
				w.Status.Phase = meithealv1alpha1.WorkloadPhaseCompleted
				_ = r.c.Status().Update(context.Background(), &w)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func (r *roundEnv) assertNewRound(t *testing.T, intent string) {
	t.Helper()
	w := getWorkload(t, r.c, plannedWorkloadName(53))
	if w.Annotations[runAttemptAnnotation] != "1" || !w.Spec.ReviseExistingBranch || w.Spec.Intent != intent {
		t.Fatalf("Workload = annotations %v, revise %v, intent %q; want attempt 1 revising the branch with %q",
			w.Annotations, w.Spec.ReviseExistingBranch, w.Spec.Intent, intent)
	}
	if len(r.gate.calls) != 1 || r.gate.calls[0].branch != r.branch {
		t.Fatalf("gate calls = %+v, want the new round verified on %s", r.gate.calls, r.branch)
	}
	led := r.ledger(t)[53]
	if len(led) != 1 || led[0].FinalStage != ledgerDone {
		t.Fatalf("ledger for 53 = %+v, want one done record", led)
	}
	if _, ok := r.decision(t, 53, ParkEscalate); ok {
		t.Fatal("the answered decision is still there")
	}
	if left, _ := filepath.Glob(filepath.Join(r.cfg.DecisionsDir, "*"+replanSuffix)); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// A1: decide on a spent round deletes the attempt-2 Workload and recreates it
// at attempt 1 with the re-planned intent, revising the pushed branch, and
// drives it without a preflight that would mistake our own PR for others'.
func TestRun_AnswerDecideStartsANewRound(t *testing.T) {
	r := newRoundEnv(t)
	opened := r.park(t, 53, ParkEscalate, string(StageVerify), true)
	r.loopEnv.forge.answer(53, "/meitheal decide keep the old wire format", opened.Add(time.Minute))
	stub := replanEnv(t, r.loopEnv, plannedAnswer("cpu", "src/lib.rs"))
	items := r.queue(t, QueueItem{Issue: 53})
	r.completeNewRound(t)

	r.run(t, items)

	if !strings.Contains(stub.prompts[53], "keep the old wire format") {
		t.Fatal("the planner did not get the decision")
	}
	r.assertNewRound(t, "## Intent\n\nChange the thing. Mirror `src/lib.rs`.")
}

func TestRun_AnswerRequeueStartsANewRoundWithTheOldIntent(t *testing.T) {
	r := newRoundEnv(t)
	opened := r.park(t, 53, ParkEscalate, string(StageVerify), true)
	r.loopEnv.forge.answer(53, "/meitheal requeue", opened.Add(time.Minute))
	items := r.queue(t, QueueItem{Issue: 53})
	r.completeNewRound(t)

	r.run(t, items)

	r.assertNewRound(t, "intent for 53")
}

// A preflight skip is not done: it is ledgered as skipped with its reason,
// and what runs after it waits, with one log line saying why.
func TestRun_PreflightSkipIsNotDone(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"skip"}}
	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53, After: []int32{52}})
	env.run(t, items)
	led := env.ledger(t)
	if len(led[52]) != 1 || led[52][0].FinalStage != ledgerSkipped ||
		!strings.Contains(led[52][0].Reason, "an open PR already references it") {
		t.Fatalf("ledger for 52 = %+v, want skipped with the reason", led[52])
	}
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{52}) {
		t.Fatalf("driven = %v, want 53 held by the skipped 52", got)
	}
	if n := env.logs.count("issue 53 waits on issue 52"); n != 1 {
		t.Fatalf("wait logged %d times, want once:\n%s", n, env.logs)
	}
	// Not finished: a restart runs preflight again.
	env.run(t, items)
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{52, 52}) {
		t.Fatalf("driven after a restart = %v, want 52's preflight again", got)
	}
}

func TestReadLedger_IgnoresATornLastLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	body := `{"issue":52,"finalStage":"done"}` + "\n" + `{"issue":53,"finalSt`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &logSink{}
	last, err := readLedger(p, "", logs.logf)
	if err != nil || last[52].FinalStage != ledgerDone || len(last) != 1 {
		t.Fatalf("readLedger = %v, %v; want 52 and the torn line ignored", last, err)
	}
	if logs.count("line 2") != 1 {
		t.Fatalf("log = %s, want the torn line named", logs)
	}
	// The next append starts on its own line, so the ledger reads cleanly.
	if err := NewLedger(p).Append(LedgerRecord{Issue: 54, FinalStage: ledgerDone}); err != nil {
		t.Fatal(err)
	}
	if last, err = readLedger(p, "", logs.logf); err != nil || last[54].FinalStage != ledgerDone {
		t.Fatalf("after an append: %v, %v", last, err)
	}
}

func TestRun_DropWithoutAForgeWaits(t *testing.T) {
	env := newLoopEnv(t)
	env.park(t, 54, ParkEscalate, string(StageVerify), true)
	if err := AnswerDecision(env.cfg.DecisionsDir, 54, ParkEscalate, "drop"); err != nil {
		t.Fatal(err)
	}
	env.cfg.Forge = nil
	env.cfg.Answerer = ""
	env.run(t, env.queue(t, QueueItem{Issue: 54}))
	if _, err := os.Stat(env.cfg.LedgerPath); !os.IsNotExist(err) {
		t.Fatal("dropped without a forge to comment with")
	}
	if env.logs.count("no forge") != 1 {
		t.Fatalf("log = %s", env.logs)
	}
}

// flakyForge fails the next failComments issue comments and counts the
// comment reads.
type flakyForge struct {
	*loopForge
	failMu       sync.Mutex
	failComments int
	reads        int
}

func (f *flakyForge) CommentsSince(
	ctx context.Context, repo string, issue int32, since time.Time,
) ([]IssueComment, error) {
	f.failMu.Lock()
	f.reads++
	f.failMu.Unlock()
	return f.loopForge.CommentsSince(ctx, repo, issue, since)
}

func (f *flakyForge) CommentIssue(ctx context.Context, repo string, issue int32, body string) error {
	f.failMu.Lock()
	if f.failComments > 0 {
		f.failComments--
		f.failMu.Unlock()
		return errors.New("forge unavailable")
	}
	f.failMu.Unlock()
	return f.loopForge.CommentIssue(ctx, repo, issue, body)
}

// noticeEnv announces a planner park for 60 through the real cluster
// Effects, with a webhook that answers status codes from codes in turn (the
// last repeats).
func noticeEnv(t *testing.T, failComments int, codes ...int) (*loopEnv, *flakyForge, *int) {
	t.Helper()
	env := newLoopEnv(t)
	var mu sync.Mutex
	pings := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(codes[min(pings, len(codes)-1)])
		pings++
	}))
	t.Cleanup(srv.Close)
	forge := &flakyForge{loopForge: env.forge, failComments: failComments}
	o := fastOpts()
	o.NotifyURL = srv.URL
	env.cfg.Forge = forge
	env.cfg.Answerer = "" // only Notify reads comments here
	env.cfg.Effects = NewClusterEffects(newEffectsClient(t, nil), effNS, forge, neutralTemplate(), &fakeGateRunner{}, o)
	env.park(t, 60, ParkNeedsSplit, intentsStage, false)
	return env, forge, &pings
}

// A2: a comment that fails is retried on the next cycle and posted once.
func TestRun_FailedParkCommentIsRetriedAndPostedOnce(t *testing.T) {
	env, forge, _ := noticeEnv(t, 1, http.StatusNoContent)
	l := newRunLoop(env.cfg)
	l.answerCycle(context.Background())
	if d, _ := env.decision(t, 60, ParkNeedsSplit); d.Notified != nil || len(forge.postedTo(60)) != 0 {
		t.Fatalf("after a failed comment: notified %v, posted %q; want neither", d.Notified, forge.postedTo(60))
	}
	l.answerCycle(context.Background())
	l.answerCycle(context.Background())
	if got := forge.postedTo(60); len(got) != 1 || !strings.HasPrefix(got[0], parkCommentPrefix) {
		t.Fatalf("posted = %q, want the park comment exactly once", got)
	}
	if d, _ := env.decision(t, 60, ParkNeedsSplit); d.Notified == nil {
		t.Fatal("park not stamped once every notice went out")
	}
}

// A2: a failed ping is retried without posting the comment again.
func TestRun_FailedPingIsRetriedWithoutRepostingTheComment(t *testing.T) {
	env, forge, pings := noticeEnv(t, 0, http.StatusBadGateway, http.StatusNoContent)
	l := newRunLoop(env.cfg)
	l.answerCycle(context.Background())
	d, _ := env.decision(t, 60, ParkNeedsSplit)
	if d.Notified != nil || d.Notices == nil || d.Notices.Comment == nil || d.Notices.Ping != nil {
		t.Fatalf("after a failed ping: %+v / %+v; want the comment recorded and the ping not", d.Notified, d.Notices)
	}
	l.answerCycle(context.Background())
	l.answerCycle(context.Background())
	if got := forge.postedTo(60); len(got) != 1 {
		t.Fatalf("posted = %q, want the comment once", got)
	}
	if *pings != 2 {
		t.Fatalf("pings = %d, want the failed one and its retry", *pings)
	}
	// The comment is recorded as sent, so the retry does not even look for it.
	if forge.reads != 1 {
		t.Fatalf("comment reads = %d, want one: the retry skips the recorded comment", forge.reads)
	}
	if d, _ := env.decision(t, 60, ParkNeedsSplit); d.Notified == nil {
		t.Fatal("park not stamped after the ping went out")
	}
}

// A2: the park comment is not posted twice when the first one went out but
// was never recorded (a lost response, a crash before the stamp).
func TestNotify_ParkCommentAlreadyThereIsNotPostedAgain(t *testing.T) {
	forge := newLoopForge()
	opened := time.Now().Add(-time.Hour)
	forge.comments[60] = []IssueComment{
		{Author: "meitheal-bot", Body: parkCommentPrefix + "stalled.", Created: opened.Add(-time.Hour)},
	}
	e := NewClusterEffects(newEffectsClient(t, nil), effNS, forge, neutralTemplate(), &fakeGateRunner{}, fastOpts())
	d := Decision{Issue: 60, Kind: ParkEscalate, Reason: "stalled", Opened: opened, Repo: loopRepo}
	// An earlier park's identical line, before this park opened, does not count.
	if _, err := e.Notify(context.Background(), QueueItem{Issue: 60, Repo: loopRepo}, d); err != nil {
		t.Fatal(err)
	}
	n, err := e.Notify(context.Background(), QueueItem{Issue: 60, Repo: loopRepo}, d)
	if err != nil || n.Comment == nil {
		t.Fatalf("second Notify = %+v, %v; want the comment counted as done", n, err)
	}
	if got := forge.postedTo(60); len(got) != 1 {
		t.Fatalf("posted = %q, want one comment for this park", got)
	}
}

// writeRawDecision writes a decision file as another run, or an older
// release, would have.
func writeRawDecision(t *testing.T, dir string, d Decision) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := sigsyaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decisionPath(dir, d.Issue, d.Kind), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A3: a park written for another repository is not this run's: it does not
// park the same-numbered issue here, it is not announced, and an answer on
// this repository's issue of that number does not answer it.
func TestRun_AnotherRepositorysParkIsLeftAlone(t *testing.T) {
	env := newLoopEnv(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeRawDecision(t, env.cfg.DecisionsDir, Decision{
		Repo: "example/other", Issue: 52, Kind: ParkEscalate, Stage: string(StageWatch), Opened: opened,
		Reason: "stalled", Options: optionsFor(ParkEscalate),
	})
	env.forge.answer(52, "/meitheal drop", opened.Add(time.Minute))
	env.run(t, env.queue(t, QueueItem{Issue: 52}))

	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{52}) {
		t.Fatalf("driven = %v, want this repository's 52 driven", got)
	}
	if n := env.effects.notifications(); len(n) != 0 {
		t.Fatalf("notified = %+v, want the other repository's park left alone", n)
	}
	d, ok := env.decision(t, 52, ParkEscalate)
	if !ok || d.Answer != "" || d.Notified != nil {
		t.Fatalf("other repository's decision = %+v, %v; want it untouched", d, ok)
	}
	if got := env.forge.postedTo(52); len(got) != 0 {
		t.Fatalf("posted %q", got)
	}
}

// A3: a park with no repository outside the queue may be another run's: it
// is neither announced nor answered.
func TestRun_LegacyParkOutsideTheQueueIsLeftAlone(t *testing.T) {
	env := newLoopEnv(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	writeRawDecision(t, env.cfg.DecisionsDir, Decision{
		Issue: 70, Kind: ParkEscalate, Stage: string(StageWatch), Opened: opened,
		Reason: "stalled", Options: optionsFor(ParkEscalate),
	})
	env.forge.answer(70, "/meitheal drop", opened.Add(time.Minute))
	env.run(t, nil)
	if n := env.effects.notifications(); len(n) != 0 {
		t.Fatalf("notified = %+v", n)
	}
	if d, _ := env.decision(t, 70, ParkEscalate); d.Answer != "" {
		t.Fatalf("legacy park outside the queue was answered: %+v", d)
	}
	if _, err := os.Stat(env.cfg.LedgerPath); !os.IsNotExist(err) {
		t.Fatal("legacy park outside the queue was acted on")
	}
}

// A3: a ledger record for another repository does not finish this one's
// issue of the same number.
func TestRun_AnotherRepositorysLedgerIsIgnored(t *testing.T) {
	env := newLoopEnv(t)
	if err := NewLedger(env.cfg.LedgerPath).Append(LedgerRecord{
		Repo: "example/other", Issue: 52, FinalStage: ledgerDone,
	}); err != nil {
		t.Fatal(err)
	}
	env.run(t, env.queue(t, QueueItem{Issue: 52}))
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{52}) {
		t.Fatalf("driven = %v, want 52 driven: the done record is another repository's", got)
	}
	if led := env.ledger(t); len(led[52]) != 2 {
		t.Fatalf("ledger = %+v", led)
	}
	for _, l := range ledgerLines(t, env.cfg.LedgerPath)[1:] {
		if l["repo"] != loopRepo {
			t.Fatalf("new record %v does not name the repository", l)
		}
	}
}

// A3: every park the loop writes names its repository.
func TestRun_ParksNameTheirRepository(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"park"}}
	env.run(t, env.queue(t, QueueItem{Issue: 52}))
	if d, ok := env.decision(t, 52, ParkEscalate); !ok || d.Repo != loopRepo {
		t.Fatalf("decision = %+v, want repo %q", d, loopRepo)
	}
}
