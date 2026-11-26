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
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sigsyaml "sigs.k8s.io/yaml"
)

const (
	// answerPollInterval is how often a run that is not --once reads park
	// answers from the issues.
	answerPollInterval = 5 * time.Minute
	// dropComment is the one public line a drop answer posts.
	dropComment = "Meitheal dropped this issue at the maintainer's request."
	// replanSuffix marks an answered decision set aside while its issue is
	// re-planned. Not ".yaml", so ListDecisions and the park guard never see
	// it; restoreReplanBackups puts it back after a crash.
	replanSuffix = ".replanning"
)

// replanFunc plans one issue again with the maintainer's decision added to
// the planner input ("" for none). It returns the planned item, or nil when
// the planner parked the issue again (the new park is already on disk). An
// error means nothing was decided and the answer should be tried again.
type replanFunc func(ctx context.Context, issue int32, decision string) (*QueueItem, error)

// runLoopConfig is everything a live run is wired from.
type runLoopConfig struct {
	Effects      Effects
	Forge        RunForge
	DecisionsDir string
	LedgerPath   string
	// Answerer is the one login whose '/meitheal' comments answer a park.
	// Empty reads no comments; answers then come only from
	// 'meitheal decisions answer'.
	Answerer string
	// Repo is the run's one repository: answers are read and parks
	// announced there.
	Repo     string
	Baseline time.Duration
	Factor   float64
	Limits   map[string]int
	// PollEvery is how often answers are read when not Once. Zero: 5m.
	PollEvery time.Duration
	// Once drives to idle, reads answers, and exits when nothing more can
	// start.
	Once bool
	// QueueFile, when set, is rewritten whenever a re-plan adds or changes
	// an item, so a restart drives the re-planned intent.
	QueueFile string
	// Replan is nil when no planner is configured; a decide answer then
	// waits, with one log line saying why.
	Replan replanFunc
	Now    func() time.Time
	Logf   func(format string, a ...any)
}

type itemState int

const (
	statePending itemState = iota
	stateRunning
	stateParked
	stateDone
	stateFailed
	// stateSkipped: preflight found the work already in flight elsewhere.
	// It does not release what runs after the item.
	stateSkipped
)

// runLoop is a live run's state. Everything but the drives themselves runs
// on the goroutine that called run.
type runLoop struct {
	cfg    runLoopConfig
	ledger *Ledger
	sched  *laneScheduler
	items  map[int32]QueueItem
	order  []int32
	state  map[int32]itemState
	// finished holds every issue that is done for good: from the ledger at
	// start, and from this run. An item runs only after everything in its
	// After list is here.
	finished map[int32]bool
	logged   map[string]bool
	// skipReason holds why preflight skipped an item, for the log line a
	// waiting dependent gets.
	skipReason map[int32]string

	// mu guards the two maps the lane goroutines share with this one.
	mu sync.Mutex
	// restarts holds the items a maintainer's answer sent round again, by
	// the answered file to remove once Restart has succeeded. Until then the
	// answer stays on disk, so a crash in between acts on it again.
	restarts map[int32]string
	// skips holds preflight skip reasons a drive reported, for finish.
	skips map[int32]string
	// stale holds answered files a new round consumed but could not remove,
	// by path; the answer cycle retries them and never acts on them again.
	stale map[string]int32
}

func newRunLoop(cfg runLoopConfig) *runLoop {
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = answerPollInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	l := &runLoop{
		cfg:        cfg,
		ledger:     NewLedger(cfg.LedgerPath),
		items:      map[int32]QueueItem{},
		state:      map[int32]itemState{},
		finished:   map[int32]bool{},
		logged:     map[string]bool{},
		skipReason: map[int32]string{},
		restarts:   map[int32]string{},
		skips:      map[int32]string{},
		stale:      map[string]int32{},
	}
	l.sched = newLaneScheduler(cfg.Limits, l.drive)
	return l
}

// runQueueLoop drives items until the run is idle (Once) or the context is
// cancelled. A cancelled context is a clean stop, not an error: in-flight
// drives return at their next poll without parking, their Workloads stay,
// and a restart resumes them.
func runQueueLoop(ctx context.Context, cfg runLoopConfig, items []QueueItem) error {
	return newRunLoop(cfg).run(ctx, items)
}

func (l *runLoop) logf(format string, a ...any) { l.cfg.Logf(format, a...) }

// logOnce logs a line the first time key is seen, so a condition that holds
// on every answer cycle is reported once per process, not every 5 minutes.
func (l *runLoop) logOnce(key, format string, a ...any) {
	if l.logged[key] {
		return
	}
	l.logged[key] = true
	l.logf(format, a...)
}

// checkOneRepo refuses items outside the run's repository. Decision files do
// not name a repository, so answers are read from one; an item elsewhere
// would have its issue number's comments read in the wrong place.
func checkOneRepo(items []QueueItem, repo string) error {
	for _, it := range items {
		if it.Repo != repo {
			return fmt.Errorf("queue: issue %d is in %q but the run reads answers from %q; "+
				"run one repository per run (and per decisions directory)", it.Issue, it.Repo, repo)
		}
	}
	return nil
}

func (l *runLoop) run(ctx context.Context, items []QueueItem) error {
	if err := CheckQueueOrder(items); err != nil {
		return err
	}
	if err := checkOneRepo(items, l.cfg.Repo); err != nil {
		return err
	}
	last, err := readLedger(l.cfg.LedgerPath, l.cfg.Repo, l.cfg.Logf)
	if err != nil {
		return err
	}
	for issue, r := range last {
		if ledgerFinished(r) {
			l.finished[issue] = true
		}
	}
	if err := restoreReplanBackups(l.cfg.DecisionsDir); err != nil {
		return err
	}
	parked, err := parkedIssues(l.cfg.DecisionsDir, l.cfg.Repo)
	if err != nil {
		return err
	}
	for _, it := range items {
		l.put(it)
		switch {
		case l.finished[it.Issue]:
			l.state[it.Issue] = stateDone
		case parked[it.Issue]:
			l.state[it.Issue] = stateParked
		default:
			l.state[it.Issue] = statePending
		}
	}
	if len(items) == 0 {
		l.logf("queue empty: reading answers to parked decisions only")
	}
	if l.cfg.Answerer == "" {
		l.logf("no --answerer: park answers are read only from 'meitheal decisions answer'")
	}
	// Answers given while the loop was down come first: a requeue answered
	// overnight should not wait for the first poll.
	l.answerCycle(ctx)
	return l.loop(ctx)
}

func (l *runLoop) loop(ctx context.Context) error {
	var tick <-chan time.Time
	if !l.cfg.Once {
		t := time.NewTicker(l.cfg.PollEvery)
		defer t.Stop()
		tick = t.C
	}
	for {
		l.startReady(ctx)
		if l.sched.busy() == 0 {
			if ctx.Err() != nil {
				l.logf("stopped; a restart resumes what was in flight")
				return nil
			}
			if l.cfg.Once {
				// Idle: one more look at the answers, which may release
				// something, and exit if nothing could start.
				l.answerCycle(ctx)
				l.startReady(ctx)
				if l.sched.busy() == 0 {
					l.logf("idle: %s", l.summary())
					return nil
				}
			}
		}
		select {
		case r := <-l.sched.results:
			l.finish(ctx, r)
		case <-tick:
			l.answerCycle(ctx)
		case <-ctx.Done():
			for l.sched.busy() > 0 {
				l.finish(ctx, <-l.sched.results)
			}
		}
	}
}

func (l *runLoop) summary() string {
	n := map[itemState]int{}
	for _, issue := range l.order {
		n[l.state[issue]]++
	}
	return fmt.Sprintf("%d done, %d parked, %d skipped, %d stopped on an error, %d waiting", n[stateDone],
		n[stateParked], n[stateSkipped], n[stateFailed], n[statePending])
}

// put adds or replaces an item, keeping queue order.
func (l *runLoop) put(it QueueItem) {
	if _, ok := l.items[it.Issue]; !ok {
		l.order = append(l.order, it.Issue)
	}
	l.items[it.Issue] = it
}

func (l *runLoop) depsDone(it QueueItem) bool {
	for _, a := range it.After {
		if !l.finished[a] {
			if l.state[a] == stateSkipped {
				l.logOnce(fmt.Sprintf("wait-skip %d/%d", it.Issue, a),
					"issue %d waits on issue %d, which preflight skipped (%s); it starts once %d is finished",
					it.Issue, a, l.skipReason[a], a)
			}
			return false
		}
	}
	return true
}

func (l *runLoop) startReady(ctx context.Context) {
	var pending []QueueItem
	for _, issue := range l.order {
		if l.state[issue] == statePending {
			pending = append(pending, l.items[issue])
		}
	}
	l.sched.startReady(ctx, pending, l.depsDone)
	for _, it := range pending {
		if l.sched.has(it.Issue) {
			l.state[it.Issue] = stateRunning
		}
	}
}

func (l *runLoop) finish(ctx context.Context, r laneResult) {
	l.sched.finish(r)
	issue := r.item.Issue
	switch {
	case r.err != nil && ctx.Err() != nil:
		// Stopped by the signal: not a failure, and nothing was recorded.
		l.state[issue] = statePending
	case r.err != nil:
		l.state[issue] = stateFailed
		l.logf("issue %d stopped at %s: %v (a restart tries it again)", issue, r.stage, r.err)
	case r.stage == StageParked:
		l.state[issue] = stateParked
		l.logf("issue %d parked", issue)
	case r.stage == StageDone:
		l.mu.Lock()
		skip, skipped := l.skips[issue]
		delete(l.skips, issue)
		l.mu.Unlock()
		if skipped {
			l.state[issue] = stateSkipped
			l.skipReason[issue] = skip
			l.logf("issue %d skipped at preflight: %s", issue, skip)
			break
		}
		l.state[issue] = stateDone
		l.finished[issue] = true
		l.logf("issue %d done", issue)
	default:
		l.state[issue] = stateFailed
		l.logf("issue %d ended at unexpected stage %s", issue, r.stage)
	}
}

// drive runs one item through DriveItem (or RestartItem, for a round a
// maintainer's answer started) on a lane goroutine and records how it ended.
// A drive stopped by the context records nothing.
func (l *runLoop) drive(ctx context.Context, item QueueItem) (Stage, error) {
	l.mu.Lock()
	answered, restart := l.restarts[item.Issue]
	delete(l.restarts, item.Issue)
	l.mu.Unlock()
	rec := &recordingEffects{
		Effects: l.cfg.Effects, dir: l.cfg.DecisionsDir, now: l.cfg.Now, logf: l.cfg.Logf,
		answered: answered, stale: l.markStale,
	}
	started := l.cfg.Now()
	stage := StagePreflight
	intent, err := Queue{}.IntentFor(item)
	if err == nil {
		run := DriveItem
		if restart {
			run = RestartItem
		}
		stage, err = run(ctx, rec, item, intent, l.cfg.DecisionsDir, l.cfg.Baseline, l.cfg.Factor)
	}
	if err != nil && ctx.Err() != nil {
		return stage, err
	}
	r := LedgerRecord{
		Repo: item.Repo, Issue: item.Issue, Lane: laneOf(item), Workload: rec.workload, Attempts: rec.attempts,
		PRURL: rec.prURL, Started: started.UTC(), Ended: l.cfg.Now().UTC(),
	}
	switch {
	case err != nil:
		r.FinalStage, r.Reason = ledgerError, fmt.Sprintf("stopped at %s: %v", stage, err)
	case stage == StageParked:
		r.FinalStage = ledgerParked
		if rec.parked != nil {
			r.ParkKind, r.Reason = rec.parked.Kind, rec.parked.Reason
		}
	case stage == StageDone && rec.skip != "":
		r.FinalStage, r.Reason = ledgerSkipped, rec.skip
		l.mu.Lock()
		l.skips[item.Issue] = rec.skip
		l.mu.Unlock()
	case stage == StageDone:
		r.FinalStage, r.Reason = ledgerDone, "finalized"
	default:
		r.FinalStage, r.Reason = ledgerError, fmt.Sprintf("ended at unexpected stage %s", stage)
	}
	if lerr := l.ledger.Append(r); lerr != nil {
		l.cfg.Logf("issue %d: %v", item.Issue, lerr)
	}
	return stage, err
}

// recordingEffects wraps the run's Effects for one drive: it keeps what the
// ledger records, and stamps a park as notified once Notify has run, so the
// answer cycle does not announce it a second time.
type recordingEffects struct {
	Effects
	dir  string
	now  func() time.Time
	logf func(format string, a ...any)
	// answered is the answer file the new round consumes: removed once the
	// round has an outcome past its start (a Workload, or a preflight
	// skip). stale is told when that removal fails, for a retry.
	answered string
	stale    func(path string, issue int32)

	skip, workload, prURL string
	attempts              int
	parked                *Decision
}

// consume removes the answer this round was started by, once.
func (r *recordingEffects) consume(issue int32) {
	if r.answered == "" {
		return
	}
	path := r.answered
	r.answered = ""
	if err := removeAnswered(path); err != nil {
		r.logf("issue %d: remove the answered decision (retried next cycle): %v", issue, err)
		if r.stale != nil {
			r.stale(path, issue)
		}
	}
}

func (r *recordingEffects) Preflight(ctx context.Context, item QueueItem, branch string) (string, error) {
	skip, err := r.Effects.Preflight(ctx, item, branch)
	r.skip = skip
	if err == nil && skip != "" {
		r.consume(item.Issue)
	}
	return skip, err
}

func (r *recordingEffects) Dispatch(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	w, attempt, err := r.Effects.Dispatch(ctx, item, intent)
	if err == nil {
		r.workload, r.attempts = w, attempt
		r.consume(item.Issue)
	}
	return w, attempt, err
}

func (r *recordingEffects) Restart(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	w, attempt, err := r.Effects.Restart(ctx, item, intent)
	if err != nil {
		return w, attempt, err
	}
	r.workload, r.attempts = w, attempt
	r.consume(item.Issue)
	return w, attempt, nil
}

func (r *recordingEffects) Watch(
	ctx context.Context, workload string, baseline time.Duration, factor float64,
) (WatchResult, error) {
	res, err := r.Effects.Watch(ctx, workload, baseline, factor)
	if res.PRURL != "" {
		r.prURL = res.PRURL
	}
	return res, err
}

func (r *recordingEffects) Redispatch(ctx context.Context, item QueueItem, workload, intent, evidence string) error {
	err := r.Effects.Redispatch(ctx, item, workload, intent, evidence)
	if err == nil {
		r.attempts++
	}
	return err
}

// Notify records which notices went out. The park counts as announced only
// when all of them did; the answer cycle retries the rest. The error is the
// driver's to log.
func (r *recordingEffects) Notify(ctx context.Context, item QueueItem, d Decision) (ParkNotices, error) {
	r.parked = &d
	n, err := r.Effects.Notify(ctx, item, d)
	if serr := RecordNotices(r.dir, d.Issue, d.Kind, n, err == nil, r.now()); serr != nil {
		r.logf("issue %d: record notices: %v", d.Issue, serr)
	}
	return n, err
}

// owns reports whether a decision is this run's to announce and answer: it
// names the run's repository, or it names none (written before decisions
// recorded one) and its issue is in the run's queue. A legacy park outside
// the queue may belong to another repository's run sharing the directory.
func (l *runLoop) owns(d Decision) bool {
	if d.Repo != "" {
		return d.Repo == l.cfg.Repo
	}
	_, queued := l.items[d.Issue]
	return queued
}

// itemFor is the queue item for an issue, or a minimal one in the run's
// repository for a park that never reached the queue (a planner park).
func (l *runLoop) itemFor(issue int32) QueueItem {
	if it, ok := l.items[issue]; ok {
		return it
	}
	return QueueItem{Issue: issue, Repo: l.cfg.Repo, Lane: laneCPU}
}

// answerCycle reads park answers, acts on every answered decision whose
// issue is not in flight, and announces every park not yet announced.
func (l *runLoop) answerCycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	l.retryStale()
	if l.cfg.Answerer != "" && l.cfg.Forge != nil {
		_, rejected, err := pollAnswers(ctx, l.cfg.Forge, l.cfg.DecisionsDir, l.cfg.Repo, l.cfg.Answerer, l.owns)
		if err != nil {
			l.logf("reading answers: %v", err)
		}
		for _, r := range rejected {
			key := fmt.Sprintf("rejected %d/%s/%s/%s", r.Issue, r.Kind, r.Command, r.Created.UTC().Format(time.RFC3339Nano))
			l.logOnce(key, "issue %d (%s): ignored '/meitheal %s' from %s: %s",
				r.Issue, r.Kind, r.Command, r.Created.UTC().Format(time.RFC3339), r.Reason)
		}
	}
	ds, err := ListDecisions(l.cfg.DecisionsDir)
	if err != nil {
		l.logOnce("list "+err.Error(), "decisions: %v", err)
	}
	for _, d := range ds {
		if ctx.Err() != nil {
			return
		}
		if d.Answer == "" || l.finished[d.Issue] || !l.owns(d) || l.isStale(d) {
			continue
		}
		// Only a parked item, or a park outside the queue, takes an answer
		// now. One in flight (parked a moment ago) is answered next cycle.
		if st, known := l.state[d.Issue]; known && st != stateParked {
			continue
		}
		l.act(ctx, d)
	}
	l.notifyParked(ctx)
}

func (l *runLoop) markStale(path string, issue int32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stale[path] = issue
}

// retryStale removes the answered files a new round consumed but could not
// remove. Left in place, one would be acted on as a fresh answer, or keep
// the round's own park of the same kind from being written. When it held
// back such a park, the item has nothing on disk to answer, so it is driven
// again: preflight finds its Workload and the round parks afresh.
func (l *runLoop) retryStale() {
	l.mu.Lock()
	stale := make(map[string]int32, len(l.stale))
	for p, n := range l.stale {
		stale[p] = n
	}
	l.mu.Unlock()
	for path, issue := range stale {
		if err := removeAnswered(path); err != nil {
			l.logf("issue %d: remove the answered decision (retried next cycle): %v", issue, err)
			continue
		}
		l.mu.Lock()
		delete(l.stale, path)
		l.mu.Unlock()
		if l.state[issue] != stateParked {
			continue
		}
		if parked, err := parkedIssues(l.cfg.DecisionsDir, l.cfg.Repo); err == nil && !parked[issue] {
			l.state[issue] = statePending
		}
	}
}

// isStale reports whether a decision file is a consumed answer awaiting
// removal.
func (l *runLoop) isStale(d Decision) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.stale[decisionPath(l.cfg.DecisionsDir, d.Issue, d.Kind)]
	return ok
}

func (l *runLoop) act(ctx context.Context, d Decision) {
	answer := strings.TrimSpace(d.Answer)
	switch {
	case answer == "requeue":
		l.requeue(ctx, d)
	case answer == "drop":
		l.drop(ctx, d)
	case answer == "hand-fix":
		l.finishByAnswer(d, ledgerDone, "hand-fixed by the maintainer")
	case answer == "decide" || strings.HasPrefix(answer, "decide:"):
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(answer, "decide"), ":"))
		if text == "" {
			l.logOnce(fmt.Sprintf("decide-empty %d/%s", d.Issue, d.Kind),
				"issue %d (%s): a decide answer needs the decision itself ('/meitheal decide <text>')",
				d.Issue, d.Kind)
			return
		}
		l.replan(ctx, d, text)
	default:
		l.logOnce(fmt.Sprintf("answer %d/%s/%s", d.Issue, d.Kind, answer),
			"issue %d (%s): answer %q is not one the run acts on (requeue, decide, drop, hand-fix)",
			d.Issue, d.Kind, answer)
	}
}

// requeue starts a new round with the item's existing intent: the next drive
// skips preflight and goes through Effects.Restart, which recreates the
// Workload at attempt 1. The decision file stays until Restart has
// succeeded. A planner park has no intent yet: requeue plans it again.
func (l *runLoop) requeue(ctx context.Context, d Decision) {
	if d.Stage == intentsStage {
		l.replan(ctx, d, "")
		return
	}
	if _, ok := l.items[d.Issue]; !ok {
		l.logOnce(fmt.Sprintf("requeue-unknown %d/%s", d.Issue, d.Kind),
			"issue %d (%s): requeue answered, but the issue is not in this run's queue", d.Issue, d.Kind)
		return
	}
	l.logf("issue %d: requeued by the maintainer; a new round starts", d.Issue)
	l.newRound(d.Issue, decisionPath(l.cfg.DecisionsDir, d.Issue, d.Kind))
}

// newRound queues an item for a round a maintainer's answer started.
// answered is the file the answer lives in, removed once Restart succeeds.
func (l *runLoop) newRound(issue int32, answered string) {
	l.mu.Lock()
	l.restarts[issue] = answered
	l.mu.Unlock()
	l.state[issue] = statePending
}

// removeAnswered deletes an answered decision (or its set-aside copy) once
// the round it started is under way.
func removeAnswered(path string) error {
	decisionsMu.Lock()
	defer decisionsMu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// replan plans the issue again with the maintainer's decision, then starts
// a new round with the new intent. The answered decision is set aside first,
// so a re-plan that parks under the same kind writes a fresh park instead of
// meeting the answered one. On an error it is put back and the answer is
// tried again next cycle. When the re-plan parks again the set-aside answer
// is removed; when it plans, the set-aside answer stays until the new
// round's Workload exists, so a crash in between re-plans on restart rather
// than resuming the old round.
func (l *runLoop) replan(ctx context.Context, d Decision, decision string) {
	if l.cfg.Replan == nil {
		l.logOnce(fmt.Sprintf("replan-off %d/%s", d.Issue, d.Kind),
			"issue %d (%s): answer %q needs the planner (--planner-url and --repo-dir); it waits until one is set",
			d.Issue, d.Kind, d.Answer)
		return
	}
	p := decisionPath(l.cfg.DecisionsDir, d.Issue, d.Kind)
	backup := p + replanSuffix
	if err := os.Rename(p, backup); err != nil {
		l.logf("issue %d: re-plan: %v", d.Issue, err)
		return
	}
	l.logf("issue %d (%s): asking the planner again for answer %q", d.Issue, d.Kind, d.Answer)
	item, err := l.cfg.Replan(ctx, d.Issue, decision)
	if err != nil {
		if rerr := restoreReplanBackup(backup); rerr != nil {
			l.logf("issue %d: %v", d.Issue, rerr)
		}
		l.logf("issue %d: re-plan failed, the answer is kept for the next cycle: %v", d.Issue, err)
		return
	}
	if item == nil {
		if err := os.Remove(backup); err != nil {
			l.logf("issue %d: remove answered decision: %v", d.Issue, err)
		}
		l.logf("issue %d: re-planned and parked again", d.Issue)
		if _, known := l.items[d.Issue]; known {
			l.state[d.Issue] = stateParked
		}
		return
	}
	if old, ok := l.items[d.Issue]; ok {
		item.After = old.After
	}
	if item.Repo == "" {
		item.Repo = l.cfg.Repo
	}
	l.put(*item)
	l.logf("issue %d: re-planned, intent %s; a new round starts", d.Issue, item.IntentPath)
	l.newRound(d.Issue, backup)
	l.persistQueue()
}

// drop posts the drop line once (a restart that sees it already there does
// not post it again), records the item as dropped and leaves the branch, PR,
// Workload and decision file where they are.
func (l *runLoop) drop(ctx context.Context, d Decision) {
	if l.cfg.Forge == nil {
		l.logOnce(fmt.Sprintf("drop-noforge %d/%s", d.Issue, d.Kind),
			"issue %d (%s): drop answered, but the run has no forge to comment with", d.Issue, d.Kind)
		return
	}
	repo := l.itemFor(d.Issue).Repo
	comments, err := l.cfg.Forge.CommentsSince(ctx, repo, d.Issue, time.Time{})
	if err != nil {
		l.logf("issue %d: drop: read comments: %v", d.Issue, err)
		return
	}
	posted := false
	for _, c := range comments {
		if strings.TrimSpace(c.Body) == dropComment {
			posted = true
			break
		}
	}
	if !posted {
		if err := l.cfg.Forge.CommentIssue(ctx, repo, d.Issue, dropComment); err != nil {
			l.logf("issue %d: drop: comment: %v", d.Issue, err)
			return
		}
	}
	l.finishByAnswer(d, ledgerDropped, "dropped at the maintainer's request")
}

// finishByAnswer records an item an answer finished. The ledger is written
// before the item counts as finished: without the record, a restart would
// drive it again.
func (l *runLoop) finishByAnswer(d Decision, stage, reason string) {
	now := l.cfg.Now().UTC()
	if err := l.ledger.Append(LedgerRecord{
		Repo: l.cfg.Repo, Issue: d.Issue, Lane: laneOf(l.itemFor(d.Issue)), Workload: d.Workload,
		FinalStage: stage, ParkKind: d.Kind, Reason: reason, Started: now, Ended: now,
	}); err != nil {
		l.logf("issue %d: %v (the answer is tried again next cycle)", d.Issue, err)
		return
	}
	l.finished[d.Issue] = true
	if _, known := l.items[d.Issue]; known {
		l.state[d.Issue] = stateDone
	}
	l.logf("issue %d: %s", d.Issue, reason)
}

// notifyParked announces every unanswered park that has not been fully
// announced (a planner park, or one whose notice failed) and records what
// went out; a failed notice is retried next cycle.
func (l *runLoop) notifyParked(ctx context.Context) {
	ds, _ := ListDecisions(l.cfg.DecisionsDir)
	for _, d := range ds {
		if ctx.Err() != nil {
			return
		}
		if d.Answer != "" || d.Notified != nil || l.finished[d.Issue] || !l.owns(d) {
			continue
		}
		if st, known := l.state[d.Issue]; known && st == stateRunning {
			continue
		}
		n, err := l.cfg.Effects.Notify(ctx, l.itemFor(d.Issue), d)
		if err != nil {
			l.logf("notify for issue %d (%s) failed, retried next cycle: %v", d.Issue, d.Kind, err)
		}
		if rerr := RecordNotices(l.cfg.DecisionsDir, d.Issue, d.Kind, n, err == nil, l.cfg.Now()); rerr != nil {
			l.logf("issue %d: record notices: %v", d.Issue, rerr)
		}
	}
}

// persistQueue rewrites the run's queue file with every item, so a restart
// drives what was re-planned.
func (l *runLoop) persistQueue() {
	if l.cfg.QueueFile == "" {
		return
	}
	q := Queue{Repo: l.cfg.Repo}
	for _, issue := range l.order {
		q.Items = append(q.Items, l.items[issue])
	}
	if err := writeQueueFile(l.cfg.QueueFile, q); err != nil {
		l.logf("queue file: %v", err)
	}
}

// writeQueueFile writes q to path atomically after checking it parses back.
func writeQueueFile(path string, q Queue) error {
	b, err := sigsyaml.Marshal(q)
	if err != nil {
		return fmt.Errorf("marshal queue: %w", err)
	}
	if _, err := ParseQueue(b); err != nil {
		return fmt.Errorf("queue does not parse: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".queue-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil { //nolint:gosec // G302: the queue is not secret
		return err
	}
	return os.Rename(tmp, path)
}

// restoreReplanBackup puts an answered decision back after a failed re-plan,
// unless a park has since been written in its place.
func restoreReplanBackup(backup string) error {
	p := strings.TrimSuffix(backup, replanSuffix)
	if _, err := os.Stat(p); err == nil {
		return os.Remove(backup)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(backup, p); err != nil {
		return fmt.Errorf("restore answered decision %s: %w", p, err)
	}
	return nil
}

// restoreReplanBackups recovers from a crash in the middle of a re-plan: the
// answer comes back and is acted on again, or, when the re-plan already
// parked the issue afresh, the set-aside answer is dropped.
func restoreReplanBackups(dir string) error {
	left, err := filepath.Glob(filepath.Join(dir, "*.yaml"+replanSuffix))
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range left {
		errs = append(errs, restoreReplanBackup(b))
	}
	return errors.Join(errs...)
}

// newReplanner re-plans one issue through the planner 'meitheal intents'
// uses, with the maintainer's decision added to that issue's input. Intents
// go to outDir; the queue file is the run loop's to write.
func newReplanner(
	f RunForge, call PlannerCaller, repo, repoDir, outDir, decisionsDir string, cfg PlanConfig,
) replanFunc {
	return func(ctx context.Context, issue int32, decision string) (*QueueItem, error) {
		c := cfg
		c.MaintainerDecisions = nil
		if decision != "" {
			c.MaintainerDecisions = map[int32]string{issue: decision}
		}
		q, err := planIssuesTo(ctx, f, call, repo, repoDir, []int32{issue}, outDir, decisionsDir, c)
		if err != nil {
			return nil, err
		}
		if len(q.Items) == 0 {
			return nil, nil
		}
		it := q.Items[0]
		it.Repo = repo
		return &it, nil
	}
}
