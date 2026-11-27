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
)

// maxDriveErrors is how many drives of one item in a row may end in an error
// before the item parks instead of being tried again.
const maxDriveErrors = 3

// DriveErrorsReason is the park reason for an item whose drives kept ending
// in an error.
const DriveErrorsReason = "the loop kept failing on this issue"

// driveFailed handles a drive that ended in an error (not a stop): the item
// is tried again on the next answer cycle, and the maxDriveErrors-th error in
// a row parks it escalate, announced like every other park, so a lasting
// failure (a forge the token cannot write to, a Workload the API refuses)
// reaches a human instead of being retried silently forever.
func (l *runLoop) driveFailed(ctx context.Context, r laneResult) {
	issue := r.item.Issue
	l.errCount[issue]++
	n := l.errCount[issue]
	if n < maxDriveErrors {
		l.state[issue] = stateFailed
		l.logf("issue %d stopped at %s: %v (tried again next answer cycle; error %d of %d before it parks)",
			issue, r.stage, r.err, n, maxDriveErrors)
		return
	}
	l.logf("issue %d stopped at %s: %v (error %d in a row; parking it)", issue, r.stage, r.err, n)
	l.mu.Lock()
	workload := l.workloads[issue]
	answered := l.restarts[issue]
	delete(l.restarts, issue)
	l.mu.Unlock()
	// A round a maintainer's answer started never got going: the answer is
	// used up, so the park below asks again instead of meeting it on disk.
	if answered != "" {
		if err := removeAnswered(answered); err != nil {
			l.logf("issue %d: remove the answered decision (retried next cycle): %v", issue, err)
			l.markStale(answered, issue)
		}
	}
	d := Decision{
		Issue:    issue,
		Workload: workload,
		Stage:    string(r.stage),
		Kind:     ParkEscalate,
		Reason:   DriveErrorsReason,
		Options:  optionsFor(ParkEscalate),
		Repo:     r.item.Repo,
		Opened:   l.cfg.Now().UTC(),
		// Private: the decision file and the webhook. The public comment
		// carries the reason only.
		Evidence: map[string]string{"error": fmt.Sprintf("stopped at %s: %v", r.stage, r.err)},
	}
	if _, err := ParkDecision(l.cfg.DecisionsDir, d); err != nil {
		// Not parked: it stays failed and is tried again, and parks on the
		// next error.
		l.state[issue] = stateFailed
		l.logf("issue %d: park after %d errors: %v (tried again next answer cycle)", issue, n, err)
		return
	}
	l.errCount[issue] = 0
	l.state[issue] = stateParked
	if err := l.ledger.Append(LedgerRecord{
		Repo: r.item.Repo, Issue: issue, Lane: laneOf(r.item), Workload: workload,
		FinalStage: ledgerParked, ParkKind: d.Kind, Reason: d.Reason, Started: d.Opened, Ended: d.Opened,
	}); err != nil {
		l.logf("issue %d: %v", issue, err)
	}
	n2, err := l.cfg.Effects.Notify(ctx, r.item, d)
	if err != nil {
		l.logf("notify for issue %d (%s) failed, retried next cycle: %v", issue, d.Kind, err)
	}
	if rerr := RecordNotices(l.cfg.DecisionsDir, issue, d.Kind, n2, err == nil, l.cfg.Now()); rerr != nil {
		l.logf("issue %d: record notices: %v", issue, rerr)
	}
}

// retryFailed puts every item whose last drive ended in an error back in
// line; the answer cycle calls it, so a failing item is tried once per cycle.
func (l *runLoop) retryFailed() {
	for _, issue := range l.order {
		if l.state[issue] == stateFailed {
			l.state[issue] = statePending
		}
	}
}

// roundStarted forgets the new-round marker of an issue once the round has
// an outcome past its start (a Workload, or a preflight skip). Until then a
// drive that fails leaves it, so the retry starts the round again rather
// than resuming the old one.
func (l *runLoop) roundStarted(issue int32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.restarts, issue)
}
