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
	"log"
	"time"
)

// WatchResult is what the watch stage observed.
type WatchResult struct {
	// Stalled is the stall predicate's verdict, IsStalled applied to what the
	// watcher saw. The driver does not re-derive it.
	Stalled bool
	// BranchPushed reports whether the coder pushed anything.
	//
	// The production Watch (run_effects.go) sets it from the coder tasks'
	// recorded branch and feeds it, with the time since the coder's claim and
	// the baseline, into IsStalled via StallInput.BranchPushed; Stalled above
	// is that verdict. The driver never reads this field.
	BranchPushed bool
	// WorkloadFailed reports that review rounds were exhausted or a task
	// failed terminally.
	WorkloadFailed bool
	// Concern is the first structured concern text from any child task, ""
	// when none.
	Concern string
	// PRURL is the pull request the run opened, when there is one.
	PRURL string
}

// Effects are the side-effecting operations the driver performs. Kept behind
// an interface so DriveItem's control flow is testable without a cluster;
// the production implementation, NewClusterEffects (run_effects.go), wraps the
// controller-runtime client, a GateRunner and a RunForge.
//
// Effects.Preflight is a METHOD and is not the package-level Preflight
// function: that one takes a PreflightProbe and does the forge reads, and the
// production implementation of this method calls it.
type Effects interface {
	Preflight(ctx context.Context, item QueueItem, branch string) (string, error)
	// Dispatch creates the item's Workload or resumes the one already there,
	// and reports which coder attempt that Workload is: 1 for a first
	// dispatch, 2 for a Workload that is already the feedback pass. The
	// attempt is read back from the cluster on a resume, so the one-pass cap
	// survives a loop restart.
	Dispatch(ctx context.Context, item QueueItem, intent string) (workload string, attempt int, err error)
	// Restart begins a new round for an item a maintainer answered with
	// requeue or decide: the live Workload (if any) is deleted and waited
	// out, its branch and draft PR kept, and the Workload is created again
	// as attempt 1, revising the branch when one was pushed. The attempt cap
	// is per round, and only a maintainer's answer starts a round.
	Restart(ctx context.Context, item QueueItem, intent string) (workload string, attempt int, err error)
	// HasRound reports whether the issue has a round of this loop's to
	// restart: its own Workload (the reserved name, with the issue in
	// Spec.Issues) or its own task branch. Without either, nothing the
	// forge shows for the issue is this loop's.
	HasRound(ctx context.Context, item QueueItem) (bool, error)
	Watch(ctx context.Context, workload string, baseline time.Duration, factor float64) (WatchResult, error)
	Kill(ctx context.Context, workload string) error
	Verify(ctx context.Context, workload, branch string) (clean bool, evidence string, err error)
	// Redispatch starts the one feedback pass on an existing Workload. evidence
	// is the previous Verify's evidence string, unmodified; truncating it for
	// the coder is the implementation's job.
	Redispatch(ctx context.Context, item QueueItem, workload, intent, evidence string) error
	// Finalize opens the pull request for a clean verify.
	Finalize(ctx context.Context, item QueueItem, workload, branch string) error
	// Notify tells a human a decision was parked, on every channel that
	// d.Notices does not already show as done, and returns the channels that
	// are done now. Each is recorded only once it succeeded, so a caller can
	// retry a failure without repeating what went out. A failure is logged
	// by the driver and never unparks the item.
	Notify(ctx context.Context, item QueueItem, d Decision) (ParkNotices, error)
}

// taskBranch is the branch Meitheal's coder pushes for a Workload. It mirrors
// the controller's own convention, which builds the name from the Workload and
// the issue: workload_controller.go:379 is "meitheal/<workload>/issue-<n>".
//
// The workload name is a parameter and not derived from the issue on purpose.
// Effects.Dispatch RETURNS the name it created precisely so the caller does not
// have to guess it, and a production Dispatch has every reason to return
// something other than the reserved name: a retry suffix, a collision-avoiding
// name, a slug carrying the repo. A driver that rebuilt the branch from the
// issue would then watch and verify a branch nobody pushed and call the verify
// clean, which is a silent wrong answer in the direction that matters.
func taskBranch(workload string, issue int32) string {
	return fmt.Sprintf("meitheal/%s/issue-%d", workload, issue)
}

// plannedWorkloadName is the Workload name the driver reserves for an item.
//
// Preflight probes the task branch before any Workload exists, so that one call
// has no returned name to work from and has to assume this. It is the only
// place the assumption is unavoidable, and it is a probe rather than a
// judgment: an open PR on the issue is the stronger of preflight's two signals
// and does not depend on the name at all.
func plannedWorkloadName(issue int32) string {
	return fmt.Sprintf("wl-%d", issue)
}

// optionsFor is the answer set a parked decision offers. Every park sets one:
// AnswerDecision accepts arbitrary text when a decision offers no options and
// `meitheal decisions` renders that as "any", so an option-less park hands the
// human a free-text prompt instead of the answers the loop understands.
//
// Every park kind offers the answer commands requeue, decide and drop;
// escalate keeps hand-fix on top, which stays a CLI-only answer.
func optionsFor(kind string) []string {
	switch kind {
	case ParkEscalate:
		return []string{"requeue", "decide", "hand-fix", "drop"}
	case ParkNeedsDecision, ParkNeedsSplit:
		return []string{"requeue", "decide", "drop"}
	}
	// A literal, NOT ParkEscalate. These are answers a human may type, not park
	// kinds, and the two vocabularies coincide on "escalate" by coincidence.
	// Coupling them would make renaming the park kind silently change the
	// option set that already-parked decisions were written with, and
	// AnswerDecision validates a typed answer against exactly that stored set.
	// Only the retired adjudicate kind reaches here.
	return []string{"accept", "revise", "escalate", "drop"}
}

// maxStageTransitions bounds one item's trip through the loop.
//
// The real machine's longest path is eight transitions (preflight, dispatch,
// watch, verify, feedback, watch, verify, finalize), so a healthy run never
// comes near this. It is a backstop against a cycle: NextStage maps
// StageFeedback back to StageWatch, so the loop below has a cycle in it and is
// saved by the one-pass rule (feedback increments attempts). A hang is
// the worst failure mode a human-facing loop has, because it is
// indistinguishable from a slow agent and the CLI never says otherwise.
const maxStageTransitions = 64

// DriveItem advances one queue item until it reaches a terminal stage: done,
// or parked. Parking is terminal for this pass by design, so the caller
// proceeds to the next item rather than blocking on a human.
//
// `meitheal run` calls it once per drive of an item, from a lane goroutine
// (run_loop.go), with the cluster Effects from NewClusterEffects.
func DriveItem(
	ctx context.Context,
	e Effects,
	item QueueItem,
	intent, decisionsDir string,
	baseline time.Duration,
	factor float64,
) (Stage, error) {
	return driveLoop(ctx, e, item, intent, decisionsDir, baseline, factor, NextStage, false)
}

// RestartItem is DriveItem for a new round a maintainer's answer started.
// When the issue has a round of this loop's (Effects.HasRound: its own
// Workload or task branch), preflight is skipped, because the open draft PR
// and the branch it would find are the loop's own, and the drive starts from
// Effects.Restart instead of Dispatch. Without one (a planner park that was
// never dispatched) it is DriveItem: preflight runs, so a PR someone else
// opened meanwhile still skips the issue. Everything after is DriveItem's.
func RestartItem(
	ctx context.Context,
	e Effects,
	item QueueItem,
	intent, decisionsDir string,
	baseline time.Duration,
	factor float64,
) (Stage, error) {
	own, err := e.HasRound(ctx, item)
	if err != nil {
		return StagePreflight, fmt.Errorf("restart issue %d: %w", item.Issue, err)
	}
	return driveLoop(ctx, e, item, intent, decisionsDir, baseline, factor, NextStage, own)
}

// driveLoop is DriveItem's body with the stage machine as a parameter.
//
// The indirection buys exactly one thing: no input reaches a cycle through the
// real NextStage, so maxStageTransitions would otherwise be untestable, and an
// untested guard against a hang is not a guard. Production passes NextStage.
func driveLoop(
	ctx context.Context,
	e Effects,
	item QueueItem,
	intent, decisionsDir string,
	baseline time.Duration,
	factor float64,
	next func(Stage, Facts) Transition,
	restart bool,
) (Stage, error) {
	// Preflight has no Workload to name yet; every stage after dispatch uses
	// the name Dispatch reported.
	branch := taskBranch(plannedWorkloadName(item.Issue), item.Issue)
	var workload, evidence string
	// attempts drives NextStage's one-pass rule: a dirty verify at attempts 1
	// goes to feedback, at maxAttempts it escalates. case StageDispatch SETS
	// it to the attempt Dispatch reports (2 when a restarted loop resumes a
	// Workload that is already the feedback pass) or Restart reports (always
	// 1: a maintainer's answer starts a new round), and case StageFeedback
	// increments it; nothing else touches it. A feedback case that did not
	// increment would spin watch -> verify -> feedback -> watch with Attempts
	// frozen, maxAttempts would never fire, and only maxStageTransitions would
	// stop it.
	attempts := 0
	stage := StagePreflight
	if restart {
		stage = StageDispatch
	}

	for i := 0; ; i++ {
		if i >= maxStageTransitions {
			return stage, fmt.Errorf(
				"run loop for issue %d made %d stage transitions without reaching a terminal "+
					"stage, stuck at %q: the stage machine has a cycle",
				item.Issue, i, stage)
		}
		facts := Facts{Attempts: attempts}
		switch stage {
		case StagePreflight:
			skip, err := e.Preflight(ctx, item, branch)
			if err != nil {
				return stage, err
			}
			facts.SkipReason = skip
		case StageDispatch:
			dispatch := e.Dispatch
			if restart {
				dispatch = e.Restart
			}
			w, attempt, err := dispatch(ctx, item, intent)
			if err != nil {
				return stage, err
			}
			// An attempt below 1 would buy the item an extra feedback pass.
			if attempt < 1 {
				return stage, fmt.Errorf("dispatch for issue %d reported attempt %d", item.Issue, attempt)
			}
			// An empty name is the one answer the "trust what Dispatch
			// returns" contract cannot absorb: it makes the branch
			// "meitheal//issue-<n>", points watch, kill and verify at nothing,
			// and parks a decision whose Workload field is blank, which is
			// the only thing telling a human which run to go and look at.
			// A Workload that cannot be named was not created.
			if w == "" {
				return stage, fmt.Errorf("dispatch for issue %d reported no workload name", item.Issue)
			}
			workload = w
			// The branch follows the Workload that actually exists, not the
			// name reserved for the probe above.
			branch = taskBranch(workload, item.Issue)
			attempts = attempt
		case StageWatch:
			res, err := e.Watch(ctx, workload, baseline, factor)
			if err != nil {
				return stage, err
			}
			if res.Stalled {
				if err := e.Kill(ctx, workload); err != nil {
					return stage, err
				}
			}
			facts.Stalled = res.Stalled
			facts.WorkloadFailed = res.WorkloadFailed
			facts.Concern = res.Concern
		case StageVerify:
			clean, ev, err := e.Verify(ctx, workload, branch)
			if err != nil {
				return stage, err
			}
			facts.VerifyClean, evidence = clean, ev
			facts.GPUBusy = !clean && ev == GPUBusyEvidence
		case StageFeedback:
			if err := e.Redispatch(ctx, item, workload, intent, evidence); err != nil {
				return stage, err
			}
			attempts++
		case StageFinalize:
			if err := e.Finalize(ctx, item, workload, branch); err != nil {
				return stage, err
			}
		}

		t := next(stage, facts)
		switch t.Next {
		case StageParked:
			d := Decision{
				Issue:    item.Issue,
				Workload: workload,
				Stage:    string(stage),
				Kind:     t.Park,
				Reason:   t.Reason,
				Options:  optionsFor(t.Park),
				Repo:     item.Repo,
				// Set here, not left to ParkDecision, so Notify sees the
				// same park time the file records.
				Opened: time.Now().UTC(),
			}
			// Only when verify actually produced some: a stall is killed
			// before verify runs, and an empty "verify" key would claim a
			// judgment that nothing made.
			if evidence != "" {
				d.Evidence = map[string]string{"verify": evidence}
			}
			if facts.Concern != "" {
				if d.Evidence == nil {
					d.Evidence = map[string]string{}
				}
				d.Evidence["concern"] = facts.Concern
			}
			// A park that could not be written is not a park. Returning
			// StageParked here would tell the caller a human has been asked
			// when there is nothing on disk to answer.
			if _, err := ParkDecision(decisionsDir, d); err != nil {
				return stage, err
			}
			// Told after the decision is on disk: a ping about a park that was
			// never written would send a human to nothing. A failed ping is a
			// missed notification, not a missed park, so it never unparks.
			if _, err := e.Notify(ctx, item, d); err != nil {
				log.Printf("notify for issue %d (%s) failed: %v", item.Issue, d.Kind, err)
			}
			return StageParked, nil
		case StageDone:
			return StageDone, nil
		}
		stage = t.Next
	}
}
