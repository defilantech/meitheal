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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	driveIssue  = int32(1602)
	driveRepo   = "defilantech/LLMKube"
	driveIntent = "fix the thing"
	// The name Dispatch returns. Deliberately not "wl-1602": a driver that
	// derives the branch from the issue instead of from this cannot be told
	// apart from a correct one when the two happen to agree.
	driveWorkload = "wl-test"
	// What preflight probes, before any Workload exists.
	drivePlannedBranch = "meitheal/wl-1602/issue-1602"
	// What every stage after dispatch acts on.
	driveDispatchedBranch = "meitheal/wl-test/issue-1602"
	// Realistic gate output: multi-line, with runs of two spaces. Nothing here
	// asserts on it through the rendered table, whose column splitter treats
	// two spaces as a break.
	driveEvidence = "gate FAIL: 2 tests\n  pkg/cli  TestOne\n  pkg/cli  TestTwo"
	// Deliberately not DefaultBaseline (60m) or DefaultStallFactor (2.5), so
	// a driver that substitutes a package default for the caller's budget is
	// visible rather than accidentally right.
	driveBaseline = 90 * time.Minute
	driveFactor   = 3.5
)

// Sentinels, one per effect, so errors.Is can tell which branch surfaced the
// failure rather than accepting any error at all.
var (
	errPreflightDown  = errors.New("forge unreachable")
	errDispatchRefsd  = errors.New("workload rejected")
	errWatchLost      = errors.New("watch stream closed")
	errKillRefused    = errors.New("delete forbidden")
	errVerifyExploded = errors.New("gate job never scheduled")
)

// driveCtxKey marks the caller's context. Each effect records the context it
// was handed, so a stage given a fresh context.Background() is visible.
type driveCtxKey struct{}

// driveCall is what one effect was handed. Unused fields stay zero: Watch has
// no branch, Preflight has no workload.
type driveCall struct {
	ctx      context.Context
	item     QueueItem
	branch   string
	intent   string
	workload string
	baseline time.Duration
	factor   float64
}

// fakeEffects answers the driver's side effects from a fixture and records
// what each one was handed. The recordings are not decoration: every one of
// them is asserted in TestDriveItem_ForwardsWhatEachStageNeeds, because a
// driver that hands Verify a blank branch still gets a clean answer back and
// the ordinary stage assertions cannot see the difference.
type fakeEffects struct {
	skip           string
	watch          WatchResult
	verifyClean    bool
	verifyEvidence string
	// verifyCleans, when set, answers successive Verify calls in order (the
	// last repeats); verifyEvidences does the same for the evidence.
	verifyCleans    []bool
	verifyEvidences []string
	// dispatchWorkload is the Workload name Dispatch reports creating.
	// Empty means driveWorkload.
	dispatchWorkload string
	// dispatchNoName makes Dispatch report ("", nil): it claims to have
	// created a Workload and does not say which.
	dispatchNoName bool
	// dispatchAttempt is the attempt Dispatch reports. Zero means 1.
	dispatchAttempt int

	redispatchErr error
	finalizeErr   error
	notifyErr     error

	redispatched []redispatchCall
	finalized    []driveCall
	notified     []Decision
	verified     int

	preflightErr error
	dispatchErr  error
	watchErr     error
	killErr      error
	verifyErr    error

	calls      map[string]driveCall
	noRound    bool
	restarted  int
	dispatched int
	killed     int
	watched    int
}

func (f *fakeEffects) record(site string, c driveCall) {
	if f.calls == nil {
		f.calls = map[string]driveCall{}
	}
	f.calls[site] = c
}

func (f *fakeEffects) Preflight(ctx context.Context, item QueueItem, branch string) (string, error) {
	f.record("preflight", driveCall{ctx: ctx, item: item, branch: branch})
	return f.skip, f.preflightErr
}

func (f *fakeEffects) Dispatch(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	f.record("dispatch", driveCall{ctx: ctx, item: item, intent: intent})
	f.dispatched++
	attempt := f.dispatchAttempt
	if attempt == 0 {
		attempt = 1
	}
	if f.dispatchErr != nil {
		return "", 0, f.dispatchErr
	}
	if f.dispatchNoName {
		return "", attempt, nil
	}
	if f.dispatchWorkload != "" {
		return f.dispatchWorkload, attempt, nil
	}
	return driveWorkload, attempt, nil
}

func (f *fakeEffects) Restart(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	f.record("restart", driveCall{ctx: ctx, item: item, intent: intent})
	f.restarted++
	if f.dispatchErr != nil {
		return "", 0, f.dispatchErr
	}
	return driveWorkload, 1, nil
}

func (f *fakeEffects) HasRound(context.Context, QueueItem) (bool, error) { return !f.noRound, nil }

func (f *fakeEffects) Watch(ctx context.Context, workload string, baseline time.Duration,
	factor float64) (WatchResult, error) {
	f.record("watch", driveCall{ctx: ctx, workload: workload, baseline: baseline, factor: factor})
	f.watched++
	return f.watch, f.watchErr
}

func (f *fakeEffects) Kill(ctx context.Context, workload string) error {
	f.record("kill", driveCall{ctx: ctx, workload: workload})
	f.killed++
	return f.killErr
}

func (f *fakeEffects) Verify(ctx context.Context, workload, branch string) (bool, string, error) {
	f.record("verify", driveCall{ctx: ctx, workload: workload, branch: branch})
	n := f.verified
	f.verified++
	clean, ev := f.verifyClean, f.verifyEvidence
	if len(f.verifyCleans) > 0 {
		clean = f.verifyCleans[min(n, len(f.verifyCleans)-1)]
	}
	if len(f.verifyEvidences) > 0 {
		ev = f.verifyEvidences[min(n, len(f.verifyEvidences)-1)]
	}
	return clean, ev, f.verifyErr
}

type redispatchCall struct {
	item                       QueueItem
	workload, intent, evidence string
}

func (f *fakeEffects) Redispatch(_ context.Context, item QueueItem, workload, intent, evidence string) error {
	f.redispatched = append(f.redispatched, redispatchCall{item, workload, intent, evidence})
	return f.redispatchErr
}

func (f *fakeEffects) Finalize(ctx context.Context, item QueueItem, workload, branch string) error {
	f.finalized = append(f.finalized, driveCall{ctx: ctx, item: item, workload: workload, branch: branch})
	return f.finalizeErr
}

func (f *fakeEffects) Notify(_ context.Context, _ QueueItem, d Decision) (ParkNotices, error) {
	f.notified = append(f.notified, d)
	return ParkNotices{}, f.notifyErr
}

func driveItemUnderTest(ctx context.Context, e *fakeEffects, dir string) (Stage, error) {
	return DriveItem(ctx, e,
		QueueItem{Issue: driveIssue, Repo: driveRepo, IntentPath: "intent.md"},
		driveIntent, dir, driveBaseline, driveFactor)
}

// drive runs the driver over one item and fails the test if it errored.
func drive(t *testing.T, e *fakeEffects, dir string) Stage {
	t.Helper()
	end, err := driveItemUnderTest(context.Background(), e, dir)
	if err != nil {
		t.Fatalf("DriveItem: %v", err)
	}
	return end
}

// noDecisions asserts nothing was parked. ListDecisions returns what it could
// parse alongside an error, so both are reported.
func noDecisions(t *testing.T, dir string) {
	t.Helper()
	ds, err := ListDecisions(dir)
	if len(ds) != 0 {
		t.Errorf("decisions = %+v, want none", ds)
	}
	if err != nil {
		t.Errorf("ListDecisions: %v", err)
	}
}

// onlyDecision returns the single decision parked in dir.
func onlyDecision(t *testing.T, dir string) Decision {
	t.Helper()
	ds, err := ListDecisions(dir)
	if err != nil {
		t.Fatalf("ListDecisions: %v (parsed %+v)", err, ds)
	}
	if len(ds) != 1 {
		t.Fatalf("decisions = %+v, want exactly one", ds)
	}
	return ds[0]
}

func TestDriveItem_SkipsAtPreflightWithoutDispatching(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{skip: "an open PR already references it"}
	if got := drive(t, e, dir); got != StageDone {
		t.Errorf("end stage = %q, want %q", got, StageDone)
	}
	if e.dispatched != 0 {
		t.Errorf("dispatched %d times, want 0", e.dispatched)
	}
	// A skip is settled, not a judgment call. Parking one would put a
	// decision in front of a human that has nothing for them to decide.
	noDecisions(t, dir)
}

func TestDriveItem_CleanRunFinishesWithoutParking(t *testing.T) {
	dir := t.TempDir()
	// Evidence even on a clean run: a passing gate still prints. Carrying it
	// must not turn a finished item into a parked one.
	e := &fakeEffects{verifyClean: true, verifyEvidence: driveEvidence}
	if got := drive(t, e, dir); got != StageDone {
		t.Errorf("end stage = %q, want %q", got, StageDone)
	}
	// Separates this path from the preflight skip, which also ends at done.
	if e.dispatched != 1 {
		t.Errorf("dispatched %d times, want 1", e.dispatched)
	}
	if e.killed != 0 {
		t.Errorf("killed %d times, want 0: a run that never stalled is not killed", e.killed)
	}
	noDecisions(t, dir)
}

func TestDriveItem_StallKillsAndParksToEscalate(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{watch: WatchResult{Stalled: true}}
	if got := drive(t, e, dir); got != StageParked {
		t.Errorf("end stage = %q, want %q", got, StageParked)
	}
	if e.killed != 1 {
		t.Errorf("killed %d times, want the stalled run killed exactly once", e.killed)
	}
	if got := e.calls["kill"].workload; got != driveWorkload {
		t.Errorf("Kill got workload %q, want %q", got, driveWorkload)
	}
	// A stall is decided before verify runs, so verify must not have run.
	if _, ran := e.calls["verify"]; ran {
		t.Error("a killed run must not then be verified")
	}
	d := onlyDecision(t, dir)
	if d.Kind != "escalate" {
		t.Errorf("Kind = %q, want escalate", d.Kind)
	}
	if d.Issue != driveIssue {
		t.Errorf("Issue = %d, want %d", d.Issue, driveIssue)
	}
	if d.Workload != driveWorkload {
		t.Errorf("Workload = %q, want %q: the human needs the run to go look at", d.Workload, driveWorkload)
	}
	if d.Stage != string(StageWatch) {
		t.Errorf("Stage = %q, want %q: the stage that parked, not the park itself", d.Stage, StageWatch)
	}
	if !strings.Contains(d.Reason, "stall") {
		t.Errorf("Reason = %q, want it to say the run stalled", d.Reason)
	}
	if want := []string{"requeue", "decide", "hand-fix", "drop"}; !slices.Equal(d.Options, want) {
		t.Errorf("Options = %q, want %q", d.Options, want)
	}
	// Nothing verified this run, so claiming verify evidence would be a lie.
	if len(d.Evidence) != 0 {
		t.Errorf("Evidence = %v, want none: verify never ran", d.Evidence)
	}
}

func TestDriveItem_DirtyVerifyGetsOneFeedbackPassThenEscalatesWithEvidence(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{verifyClean: false, verifyEvidence: driveEvidence}
	if got := drive(t, e, dir); got != StageParked {
		t.Errorf("end stage = %q, want %q", got, StageParked)
	}
	if e.killed != 0 {
		t.Errorf("killed %d times, want 0: a dirty verify is not a stall", e.killed)
	}
	if len(e.redispatched) != 1 {
		t.Errorf("redispatched %d times, want exactly one feedback pass", len(e.redispatched))
	}
	if e.dispatched != 1 {
		t.Errorf("dispatched %d times, want 1: feedback is Redispatch, not Dispatch", e.dispatched)
	}
	d := onlyDecision(t, dir)
	if d.Kind != "escalate" {
		t.Errorf("Kind = %q, want escalate", d.Kind)
	}
	if d.Stage != string(StageVerify) {
		t.Errorf("Stage = %q, want %q", d.Stage, StageVerify)
	}
	if d.Workload != driveWorkload {
		t.Errorf("Workload = %q, want %q", d.Workload, driveWorkload)
	}
	if !strings.Contains(d.Reason, "verify") {
		t.Errorf("Reason = %q, want it to name verify", d.Reason)
	}
	if want := []string{"requeue", "decide", "hand-fix", "drop"}; !slices.Equal(d.Options, want) {
		t.Errorf("Options = %q, want %q", d.Options, want)
	}
	if got := d.Evidence["verify"]; got != driveEvidence {
		t.Errorf("Evidence[verify] = %q, want %q", got, driveEvidence)
	}
	if _, err := os.Stat(filepath.Join(dir, "1602-escalate.yaml")); err != nil {
		t.Errorf("decision file not written: %v", err)
	}
}

func TestDrive_VerifyFailThenPass_OneRedispatch(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{
		verifyCleans:    []bool{false, true},
		verifyEvidences: []string{"first evidence", "second evidence"},
	}
	if got := drive(t, e, dir); got != StageDone {
		t.Fatalf("end stage = %q, want %q", got, StageDone)
	}
	if len(e.redispatched) != 1 {
		t.Fatalf("redispatched %d times, want 1", len(e.redispatched))
	}
	r := e.redispatched[0]
	if r.evidence != "first evidence" {
		t.Errorf("Redispatch evidence = %q, want the first verify's, unmodified", r.evidence)
	}
	if r.workload != driveWorkload || r.intent != driveIntent || r.item.Issue != driveIssue {
		t.Errorf("Redispatch got %+v, want the dispatched workload, the intent text and the item", r)
	}
	if e.watched != 2 || e.verified != 2 {
		t.Errorf("watched %d, verified %d, want 2 and 2", e.watched, e.verified)
	}
	if len(e.finalized) != 1 {
		t.Fatalf("finalized %d times, want 1", len(e.finalized))
	}
	f := e.finalized[0]
	if f.workload != driveWorkload || f.branch != driveDispatchedBranch || f.item.Issue != driveIssue {
		t.Errorf("Finalize got %+v, want workload %q and branch %q", f, driveWorkload, driveDispatchedBranch)
	}
	if len(e.notified) != 0 {
		t.Errorf("notified %d times, want 0: nothing parked", len(e.notified))
	}
	noDecisions(t, dir)
}

func TestDrive_VerifyFailTwice_ParksEscalateAndNotifies(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{verifyEvidence: driveEvidence}
	if got := drive(t, e, dir); got != StageParked {
		t.Fatalf("end stage = %q, want %q", got, StageParked)
	}
	if len(e.finalized) != 0 {
		t.Errorf("finalized %d times, want 0", len(e.finalized))
	}
	if len(e.notified) != 1 {
		t.Fatalf("notified %d times, want 1", len(e.notified))
	}
	n := e.notified[0]
	if n.Kind != ParkEscalate || n.Workload != driveWorkload || n.Issue != driveIssue {
		t.Errorf("Notify got %+v, want escalate on %q for issue %d", n, driveWorkload, driveIssue)
	}
}

func TestDrive_ConcernParksWithoutVerify(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{watch: WatchResult{Concern: "the spec contradicts itself"}}
	if got := drive(t, e, dir); got != StageParked {
		t.Fatalf("end stage = %q, want %q", got, StageParked)
	}
	if e.verified != 0 {
		t.Errorf("Verify ran %d times, want 0", e.verified)
	}
	d := onlyDecision(t, dir)
	if d.Kind != ParkNeedsDecision {
		t.Errorf("Kind = %q, want %q", d.Kind, ParkNeedsDecision)
	}
	if got := d.Evidence["concern"]; got != "the spec contradicts itself" {
		t.Errorf("Evidence[concern] = %q, want the concern text", got)
	}
}

func TestDrive_FailedWorkloadParksEscalateWithoutVerify(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{watch: WatchResult{WorkloadFailed: true}}
	if got := drive(t, e, dir); got != StageParked {
		t.Fatalf("end stage = %q, want %q", got, StageParked)
	}
	if e.verified != 0 || e.killed != 0 {
		t.Errorf("verified %d, killed %d, want 0 and 0", e.verified, e.killed)
	}
	d := onlyDecision(t, dir)
	if d.Kind != ParkEscalate || d.Reason != "review did not converge" {
		t.Errorf("decision = (%q,%q), want (escalate, review did not converge)", d.Kind, d.Reason)
	}
}

func TestDrive_EveryParkNotifies(t *testing.T) {
	cases := []struct {
		name     string
		effects  fakeEffects
		wantKind string
	}{
		{"stall", fakeEffects{watch: WatchResult{Stalled: true}}, ParkEscalate},
		{"concern", fakeEffects{watch: WatchResult{Concern: "c"}}, ParkNeedsDecision},
		{"failed workload", fakeEffects{watch: WatchResult{WorkloadFailed: true}}, ParkEscalate},
		{"verify twice", fakeEffects{}, ParkEscalate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.effects
			dir := t.TempDir()
			if got := drive(t, &e, dir); got != StageParked {
				t.Fatalf("end stage = %q, want parked", got)
			}
			if len(e.notified) != 1 {
				t.Fatalf("notified %d times, want exactly 1", len(e.notified))
			}
			if e.notified[0].Kind != tc.wantKind {
				t.Errorf("notified kind = %q, want %q", e.notified[0].Kind, tc.wantKind)
			}
			if want := onlyDecision(t, dir); e.notified[0].Kind != want.Kind || e.notified[0].Reason != want.Reason {
				t.Errorf("Notify got %+v, parked %+v", e.notified[0], want)
			}
		})
	}
}

// A Notify error is a missed ping, not a missed park: the decision is on disk
// and the item stays parked.
func TestDrive_NotifyErrorDoesNotUnpark(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{watch: WatchResult{Stalled: true}, notifyErr: errors.New("forge down")}
	if got := drive(t, e, dir); got != StageParked {
		t.Fatalf("end stage = %q, want parked", got)
	}
	onlyDecision(t, dir)
}

// A park that could not be written is not a park, so nobody is told about one.
func TestDrive_NoNotifyWhenParkFails(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "decisions")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &fakeEffects{watch: WatchResult{Stalled: true}}
	if _, err := driveItemUnderTest(context.Background(), e, notADir); err == nil {
		t.Fatal("want the failed park surfaced")
	}
	if len(e.notified) != 0 {
		t.Errorf("notified %d times, want 0", len(e.notified))
	}
}

func TestDrive_RedispatchAndFinalizeErrorsAbortTheItem(t *testing.T) {
	errRe := errors.New("redispatch refused")
	errFin := errors.New("finalize failed")
	dir := t.TempDir()
	e := &fakeEffects{redispatchErr: errRe}
	got, err := driveItemUnderTest(context.Background(), e, dir)
	if !errors.Is(err, errRe) || got != StageFeedback {
		t.Errorf("redispatch: (%q,%v), want (feedback, %v)", got, err, errRe)
	}
	noDecisions(t, dir)

	e = &fakeEffects{verifyClean: true, finalizeErr: errFin}
	got, err = driveItemUnderTest(context.Background(), e, dir)
	if !errors.Is(err, errFin) || got != StageFinalize {
		t.Errorf("finalize: (%q,%v), want (finalize, %v)", got, err, errFin)
	}
	noDecisions(t, dir)
}

// Every stage acts on something the driver handed it. A dropped or blanked
// argument leaves each stage answering happily about the wrong thing, which no
// stage-transition assertion can see.
func TestDriveItem_ForwardsWhatEachStageNeeds(t *testing.T) {
	ctx := context.WithValue(context.Background(), driveCtxKey{}, "carried")
	e := &fakeEffects{verifyClean: true}
	if _, err := driveItemUnderTest(ctx, e, t.TempDir()); err != nil {
		t.Fatalf("DriveItem: %v", err)
	}
	for _, site := range []string{"preflight", "dispatch", "watch", "verify"} {
		c, ok := e.calls[site]
		if !ok {
			t.Errorf("%s was never called", site)
			continue
		}
		if c.ctx == nil || c.ctx.Value(driveCtxKey{}) != "carried" {
			t.Errorf("%s got a context that is not the caller's, so cancellation cannot reach it", site)
		}
	}
	// Preflight runs before any Workload exists, so it probes the name the
	// driver reserves. Verify runs after, so it acts on the name Dispatch
	// reported. The two differ here on purpose.
	if got := e.calls["preflight"].branch; got != drivePlannedBranch {
		t.Errorf("Preflight branch = %q, want %q", got, drivePlannedBranch)
	}
	if got := e.calls["verify"].branch; got != driveDispatchedBranch {
		t.Errorf("Verify branch = %q, want %q", got, driveDispatchedBranch)
	}
	if got := e.calls["preflight"].item.Issue; got != driveIssue {
		t.Errorf("Preflight issue = %d, want %d", got, driveIssue)
	}
	// An empty repo slug is #1625: the forge answers about nothing and the
	// task branch gets cut from a stale fork HEAD.
	if got := e.calls["dispatch"].item.Repo; got != driveRepo {
		t.Errorf("Dispatch repo = %q, want %q", got, driveRepo)
	}
	// The intent, not the path to it: the coder is handed text.
	if got := e.calls["dispatch"].intent; got != driveIntent {
		t.Errorf("Dispatch intent = %q, want %q", got, driveIntent)
	}
	// Watch, kill and verify must act on the workload dispatch actually
	// created, not on a name the driver guessed.
	if got := e.calls["watch"].workload; got != driveWorkload {
		t.Errorf("Watch workload = %q, want %q", got, driveWorkload)
	}
	if got := e.calls["verify"].workload; got != driveWorkload {
		t.Errorf("Verify workload = %q, want %q", got, driveWorkload)
	}
	// The stall budget is the caller's, not a package default.
	if got := e.calls["watch"].baseline; got != driveBaseline {
		t.Errorf("Watch baseline = %v, want %v", got, driveBaseline)
	}
	if got := e.calls["watch"].factor; got != driveFactor {
		t.Errorf("Watch factor = %v, want %v", got, driveFactor)
	}
}

// Dispatch returns the Workload name it created precisely so the caller does
// not have to guess it. A retry suffix, a collision-avoiding name or a slug
// carrying the repo all produce something other than "wl-<issue>", and a driver
// that derived the branch from the issue instead would watch and verify a
// branch nobody pushed, then report a clean verify against work that is not
// there. Silent, and wrong in the direction that matters.
func TestDriveItem_TakesTheBranchFromTheWorkloadDispatchCreated(t *testing.T) {
	e := &fakeEffects{dispatchWorkload: "coder-1602-retry-7", verifyClean: true}
	if _, err := driveItemUnderTest(context.Background(), e, t.TempDir()); err != nil {
		t.Fatalf("DriveItem: %v", err)
	}
	const want = "meitheal/coder-1602-retry-7/issue-1602"
	if got := e.calls["verify"].branch; got != want {
		t.Errorf("Verify branch = %q, want %q", got, want)
	}
	// The reserved name is only ever an assumption for the one call that has
	// to happen before the Workload exists.
	if got := e.calls["preflight"].branch; got != drivePlannedBranch {
		t.Errorf("Preflight branch = %q, want %q", got, drivePlannedBranch)
	}
}

// Dispatch returning ("", nil) is the one input the "trust the name Dispatch
// returns" contract cannot absorb. Unguarded it is silent: the branch becomes
// "meitheal//issue-1602", watch and verify act on nothing, and the item parks a
// decision whose Workload field, the one thing telling a human which run to go
// look at, is blank.
func TestDriveItem_RefusesAnEmptyWorkloadNameFromDispatch(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{dispatchNoName: true}
	got, err := driveItemUnderTest(context.Background(), e, dir)
	if err == nil {
		t.Fatal("DriveItem = nil, want a Workload with no name refused")
	}
	if !strings.Contains(err.Error(), "workload") {
		t.Errorf("err = %v, want it to say the workload name is missing", err)
	}
	if got != StageDispatch {
		t.Errorf("stage = %q, want %q", got, StageDispatch)
	}
	// Nothing downstream may run against a name that is not there.
	for _, site := range []string{"watch", "kill", "verify"} {
		if _, ran := e.calls[site]; ran {
			t.Errorf("%s ran against a Workload with no name", site)
		}
	}
	// And it is a broken effect, not a judgment call for a human.
	noDecisions(t, dir)
}

// The stage machine already contains a cycle: NextStage(StageFeedback) returns
// StageWatch, and it is saved today only by nothing routing INTO feedback. The
// day a `case StageFeedback:` lands that does not re-dispatch, watch -> verify
// -> feedback -> watch spins with Attempts frozen at 1, so maxAttempts never
// fires and the CLI hangs, looking exactly like a slow agent.
//
// No input reaches that shape through the real NextStage, which is precisely
// why the backstop needs a test of its own: an untested guard against a hang is
// not a guard. Hence driveLoop taking the machine as a parameter.
func TestDriveLoop_StopsRatherThanSpinningOnACycle(t *testing.T) {
	cycle := func(cur Stage, _ Facts) Transition {
		switch cur {
		case StagePreflight:
			return Transition{Next: StageDispatch}
		case StageDispatch, StageFeedback:
			return Transition{Next: StageWatch}
		case StageWatch:
			return Transition{Next: StageVerify}
		}
		return Transition{Next: StageFeedback}
	}
	dir := t.TempDir()
	e := &fakeEffects{}
	got, err := driveLoop(context.Background(), e,
		QueueItem{Issue: driveIssue, Repo: driveRepo, IntentPath: "intent.md"},
		driveIntent, dir, driveBaseline, driveFactor, cycle, false)
	if err == nil {
		t.Fatal("driveLoop = nil, want a cycling machine refused rather than spun on")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("err = %v, want it to name what went wrong", err)
	}
	if got == StageDone || got == StageParked {
		t.Errorf("stage = %q, want a non-terminal stage: nothing finished", got)
	}
	// It really did loop, and it really did stop.
	if e.watched < 2 {
		t.Errorf("watch ran %d times, want the loop to have actually cycled", e.watched)
	}
	if e.watched >= maxStageTransitions {
		t.Errorf("watch ran %d times, want the %d-transition bound to have cut it short",
			e.watched, maxStageTransitions)
	}
	// A loop that gave up is not a judgment call for a human.
	noDecisions(t, dir)
}

// A broken effect is not a judgment call: it must abort the item, name the
// stage it broke at, and park nothing. Each case carries its own sentinel, so
// an error arriving from some other branch cannot satisfy it.
func TestDriveItem_SurfacesEffectErrorsAndParksNothing(t *testing.T) {
	cases := []struct {
		name      string
		effects   fakeEffects
		wantErr   error
		wantStage Stage
	}{
		{"preflight", fakeEffects{preflightErr: errPreflightDown}, errPreflightDown, StagePreflight},
		{"dispatch", fakeEffects{dispatchErr: errDispatchRefsd}, errDispatchRefsd, StageDispatch},
		{"watch", fakeEffects{watchErr: errWatchLost}, errWatchLost, StageWatch},
		{
			"kill",
			fakeEffects{watch: WatchResult{Stalled: true}, killErr: errKillRefused},
			errKillRefused, StageWatch,
		},
		{"verify", fakeEffects{verifyErr: errVerifyExploded}, errVerifyExploded, StageVerify},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			e := tc.effects
			got, err := driveItemUnderTest(context.Background(), &e, dir)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.wantStage {
				t.Errorf("stage = %q, want the stage that broke, %q", got, tc.wantStage)
			}
			noDecisions(t, dir)
		})
	}
}

// A park that could not be written is not a park. Reporting one anyway would
// tell the caller a human has been asked when nothing is on disk to answer.
func TestDriveItem_SurfacesAParkThatCouldNotBeWritten(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "decisions")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &fakeEffects{watch: WatchResult{Stalled: true}}
	got, err := driveItemUnderTest(context.Background(), e, notADir)
	if err == nil {
		t.Fatal("DriveItem = nil, want the failed park surfaced")
	}
	if got != StageWatch {
		t.Errorf("stage = %q, want %q: the item is not parked", got, StageWatch)
	}
}

// A busy GPU node is not a verdict on the branch: it parks escalate on the
// first attempt and never spends the feedback pass on a coder who has
// nothing to fix.
func TestDrive_GPUBusyParksWithoutFeedback(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{verifyClean: false, verifyEvidence: GPUBusyEvidence}
	if got := drive(t, e, dir); got != StageParked {
		t.Fatalf("end stage = %q, want parked", got)
	}
	if len(e.redispatched) != 0 {
		t.Fatalf("redispatched %d times, want 0", len(e.redispatched))
	}
	if len(e.notified) != 1 {
		t.Fatalf("notified %d times, want 1", len(e.notified))
	}
	d := onlyDecision(t, dir)
	if d.Kind != ParkEscalate || d.Reason != "GPU node stayed busy" {
		t.Fatalf("decision = (%q,%q), want (escalate, GPU node stayed busy)", d.Kind, d.Reason)
	}
}

// A restarted loop that resumes a Workload which is already the feedback
// pass must not grant another: the next dirty verify escalates.
func TestDrive_ResumedSecondAttemptEscalatesWithoutRedispatch(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{dispatchAttempt: 2, verifyClean: false, verifyEvidence: driveEvidence}
	if got := drive(t, e, dir); got != StageParked {
		t.Fatalf("end stage = %q, want parked", got)
	}
	if len(e.redispatched) != 0 {
		t.Fatalf("redispatched %d times, want 0 on a resumed attempt 2", len(e.redispatched))
	}
	if d := onlyDecision(t, dir); d.Kind != ParkEscalate {
		t.Fatalf("kind = %q, want escalate", d.Kind)
	}
}

func TestDrive_DispatchAttemptBelowOneIsAnError(t *testing.T) {
	dir := t.TempDir()
	e := &fakeEffects{dispatchAttempt: -1, verifyClean: false}
	if _, err := driveItemUnderTest(context.Background(), e, dir); err == nil {
		t.Fatal("want an error for a nonsense attempt")
	}
	if e.verified != 0 {
		t.Fatalf("verified %d times after a bad dispatch", e.verified)
	}
}

// A maintainer's answer starts a new round: no preflight (the open draft PR
// is the loop's own), Restart instead of Dispatch, and a fresh attempt
// count, so one feedback pass is available again.
func TestRestartItem_SkipsPreflightAndStartsAtAttemptOne(t *testing.T) {
	e := &fakeEffects{skip: "an open PR already references it: x", verifyCleans: []bool{false, true}}
	end, err := RestartItem(context.Background(), e,
		QueueItem{Issue: driveIssue, Repo: driveRepo, IntentPath: "intent.md"},
		driveIntent, t.TempDir(), driveBaseline, driveFactor)
	if err != nil || end != StageDone {
		t.Fatalf("RestartItem = %s, %v; want done", end, err)
	}
	if _, ran := e.calls["preflight"]; ran || e.dispatched != 0 || e.restarted != 1 {
		t.Fatalf("preflight ran %v, dispatched %d, restarted %d; want only one Restart", ran, e.dispatched, e.restarted)
	}
	if e.calls["restart"].intent != driveIntent {
		t.Fatalf("Restart got intent %q", e.calls["restart"].intent)
	}
	if len(e.redispatched) != 1 {
		t.Fatalf("feedback passes = %d, want the round's one", len(e.redispatched))
	}
}

// Without a round of the loop's (no own Workload, no own branch), a new
// round is an ordinary drive: preflight runs, and its skip stops it.
func TestRestartItem_WithoutARoundRunsPreflight(t *testing.T) {
	e := &fakeEffects{noRound: true, skip: "an open PR already references it: x"}
	end, err := RestartItem(context.Background(), e,
		QueueItem{Issue: driveIssue, Repo: driveRepo, IntentPath: "intent.md"},
		driveIntent, t.TempDir(), driveBaseline, driveFactor)
	if err != nil || end != StageDone {
		t.Fatalf("RestartItem = %s, %v", end, err)
	}
	if _, ran := e.calls["preflight"]; !ran || e.dispatched != 0 || e.restarted != 0 {
		t.Fatalf("preflight ran %v, dispatched %d, restarted %d; want preflight and nothing else",
			ran, e.dispatched, e.restarted)
	}
}
