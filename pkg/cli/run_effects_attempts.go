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
	"strconv"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

// runAttemptAnnotation records which coder attempt a Workload the loop
// created is: "1" from Dispatch, "2" from Redispatch. It is what lets a
// restarted loop keep the one-feedback-pass cap instead of counting from 1.
const runAttemptAnnotation = "meitheal.dev/run-attempt"

// feedbackNote is the gate-failure note on a feedback Workload's intent.
const feedbackNote = "\n\nThe previous attempt failed the independent gate. Fix this and nothing else:\n"

// recoveredFeedbackNote replaces feedbackNote when a feedback pass has to be
// recreated after a restart and the evidence it carried is gone.
const recoveredFeedbackNote = "\n\nThe previous attempt failed the independent gate. " +
	"Run the gate commands, and fix what fails and nothing else."

// deleteWaitTimeout bounds how long a delete is waited out. A var so a test
// can shorten it.
var deleteWaitTimeout = 10 * time.Minute

// attemptOf reads a Workload's coder attempt. Without the annotation (a
// Workload from before it existed, or made by hand) a revising Workload is
// taken to be the feedback pass, and anything else the first attempt.
func attemptOf(w *meithealv1alpha1.Workload) int {
	if n, err := strconv.Atoi(w.Annotations[runAttemptAnnotation]); err == nil && n >= 1 {
		return n
	}
	if w.Spec.ReviseExistingBranch {
		return 2
	}
	return 1
}

func (e *clusterEffects) newWorkload(
	name string, item QueueItem, intent string, attempt int,
) *meithealv1alpha1.Workload {
	spec := e.tpl.Spec.DeepCopy()
	spec.Intent = intent
	spec.Issues = []int32{item.Issue}
	spec.Repo = e.repoFor(item)
	spec.ReviseExistingBranch = attempt > 1
	annotations := map[string]string{runAttemptAnnotation: strconv.Itoa(attempt)}
	if item.Lane != laneGPU && e.gpuLane(name, item) {
		annotations[runLaneAnnotation] = laneGPU
	}
	return &meithealv1alpha1.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns, Annotations: annotations},
		Spec:       *spec,
	}
}

// notOurs is the refusal for a Workload under the reserved name that is not
// running this issue: resuming, deleting or recreating it would act on
// somebody else's run.
func notOurs(name string, issue int32) error {
	return fmt.Errorf("workload %s exists but is not an issue-batch run for issue %d; leaving it alone", name, issue)
}

// Dispatch creates the item's Workload under its reserved name, or resumes
// the one already there. A resumed Workload is never written to: not
// updated, not deleted, not recreated. The attempt returned is the
// Workload's own (see attemptOf), so a resumed feedback pass reports 2.
func (e *clusterEffects) Dispatch(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	name := plannedWorkloadName(item.Issue)
	w, err := e.existing(ctx, name)
	if err != nil {
		return "", 0, err
	}
	// One that runs the issue in another repository is refused whether
	// live or going away.
	own, err := e.ownedRun(w, item)
	if err != nil {
		return "", 0, fmt.Errorf("dispatch issue %d: %w", item.Issue, err)
	}
	if w != nil && w.DeletionTimestamp == nil {
		// Resuming a Workload that is not running this issue would watch,
		// verify and eventually feed back (delete and recreate) somebody
		// else's run. Refuse instead.
		if !own {
			return "", 0, notOurs(name, item.Issue)
		}
		e.adoptLane(w)
		e.remember(name, item)
		return name, attemptOf(w), nil
	}
	if own {
		e.adoptLane(w)
	}
	attempt, body := 1, intent
	if w != nil {
		// Going away: after a Kill, or mid-Redispatch when the loop died
		// between the delete and the recreate. A resume would watch a
		// corpse, so wait it out and look at the branch: a pushed branch
		// means the first attempt ran, so this is the feedback pass.
		if err := e.waitGone(ctx, name); err != nil {
			return "", 0, err
		}
		pushed, err := e.forge.BranchExists(ctx, e.repoFor(item), taskBranch(name, item.Issue))
		if err != nil {
			return "", 0, fmt.Errorf("dispatch issue %d: branch probe: %w", item.Issue, err)
		}
		if pushed {
			attempt, body = 2, intent+recoveredFeedbackNote
		}
	}
	if err := e.c.Create(ctx, e.newWorkload(name, item, body, attempt)); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", 0, fmt.Errorf("create workload %s: %w", name, err)
		}
		// Lost a race with another creator: that is a resume too, at the
		// attempt the winner recorded.
		won, gerr := e.existing(ctx, name)
		if gerr != nil || won == nil {
			return "", 0, fmt.Errorf("create workload %s: %w", name, err)
		}
		attempt = attemptOf(won)
	}
	e.remember(name, item)
	return name, attempt, nil
}

// HasRound reports whether the issue's own Workload or its own task branch
// exists. A Workload under the reserved name that runs another issue is not
// this issue's round.
func (e *clusterEffects) HasRound(ctx context.Context, item QueueItem) (bool, error) {
	name := plannedWorkloadName(item.Issue)
	w, err := e.existing(ctx, name)
	if err != nil {
		return false, err
	}
	own, err := e.ownedRun(w, item)
	if err != nil {
		return false, err
	}
	if own {
		return true, nil
	}
	pushed, err := e.forge.BranchExists(ctx, e.repoFor(item), taskBranch(name, item.Issue))
	if err != nil {
		return false, fmt.Errorf("branch probe: %w", err)
	}
	return pushed, nil
}

// Restart begins a new round for an item a maintainer answered. The live
// Workload under the reserved name is deleted in the foreground and waited
// out (bounded by deleteWaitTimeout); its branch and draft PR stay. The
// Workload is then created again as attempt 1 with intent, revising the
// branch when one was pushed, so the coder continues the draft PR instead of
// opening a second one. A Workload under the name that is not running this
// issue is somebody else's and is refused, as in Dispatch.
func (e *clusterEffects) Restart(ctx context.Context, item QueueItem, intent string) (string, int, error) {
	name := plannedWorkloadName(item.Issue)
	w, err := e.existing(ctx, name)
	if err != nil {
		return "", 0, err
	}
	if w != nil {
		own, err := e.ownedRun(w, item)
		if err != nil {
			return "", 0, fmt.Errorf("restart issue %d: %w", item.Issue, err)
		}
		if !own {
			return "", 0, notOurs(name, item.Issue)
		}
		// The branch is kept and revised, so a lane Verify gave it stays.
		e.adoptLane(w)
		if w.DeletionTimestamp == nil {
			if err := e.deleteWorkload(ctx, name); err != nil {
				return "", 0, err
			}
		}
		if err := e.waitGone(ctx, name); err != nil {
			return "", 0, err
		}
	}
	pushed, err := e.forge.BranchExists(ctx, e.repoFor(item), taskBranch(name, item.Issue))
	if err != nil {
		return "", 0, fmt.Errorf("restart issue %d: branch probe: %w", item.Issue, err)
	}
	fresh := e.newWorkload(name, item, intent, 1)
	fresh.Spec.ReviseExistingBranch = pushed
	if err := e.c.Create(ctx, fresh); err != nil {
		return "", 0, fmt.Errorf("restart issue %d: create workload %s: %w", item.Issue, name, err)
	}
	e.remember(name, item)
	return name, 1, nil
}

// lastBytes returns at most the last n bytes of s, starting on a rune
// boundary so the coder is never handed a broken character.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// Redispatch runs the one feedback pass: the Workload is deleted, waited
// out, and recreated under the same name as attempt 2 with
// ReviseExistingBranch, so the coder resumes the branch it pushed (and its
// draft PR) instead of starting over. Deleting a Workload removes its
// AgenticTasks and leaves the pushed branch alone.
//
// A Workload that is already attempt 2 is refused with nothing deleted: the
// cap is one feedback pass, whatever the caller's own count says.
func (e *clusterEffects) Redispatch(ctx context.Context, item QueueItem, workload, intent, evidence string) error {
	live, err := e.existing(ctx, workload)
	if err != nil {
		return err
	}
	if live != nil {
		own, err := e.ownedRun(live, item)
		if err != nil {
			return fmt.Errorf("redispatch issue %d: %w", item.Issue, err)
		}
		if !own {
			return notOurs(workload, item.Issue)
		}
		e.adoptLane(live)
	}
	if live != nil && attemptOf(live) >= maxAttempts {
		return fmt.Errorf("redispatch issue %d: workload %s is already attempt %d; no further feedback pass",
			item.Issue, workload, attemptOf(live))
	}
	e.remember(workload, item)
	revised := intent + feedbackNote + lastBytes(evidence, evidenceTailBytes)
	if err := e.deleteWorkload(ctx, workload); err != nil {
		return err
	}
	if err := e.waitGone(ctx, workload); err != nil {
		return err
	}
	if err := e.c.Create(ctx, e.newWorkload(workload, item, revised, 2)); err != nil {
		return fmt.Errorf("recreate workload %s: %w", workload, err)
	}
	return nil
}

// deleteWorkload deletes in the foreground: the Workload stays visible until
// its AgenticTasks are gone, so waiting for it to vanish also waits out its
// children. A recreated Workload would otherwise render task names its
// predecessor's tasks still hold.
func (e *clusterEffects) deleteWorkload(ctx context.Context, name string) error {
	w := &meithealv1alpha1.Workload{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns}}
	if err := e.c.Delete(ctx, w, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("delete workload %s: %w", name, err)
	}
	return nil
}

// waitGone polls until the Workload is gone, bounded by ctx and by
// deleteWaitTimeout so a stuck finalizer cannot hang a lane.
func (e *clusterEffects) waitGone(ctx context.Context, name string) error {
	wctx, cancel := context.WithTimeout(ctx, deleteWaitTimeout)
	defer cancel()
	for {
		w, err := e.existing(wctx, name)
		if err != nil {
			if wctx.Err() != nil && ctx.Err() == nil {
				return fmt.Errorf("workload %s still exists after %s waiting for its deletion", name, deleteWaitTimeout)
			}
			return err
		}
		if w == nil {
			return nil
		}
		if err := sleepCtx(wctx, e.o.Poll); err != nil {
			if ctx.Err() == nil {
				return fmt.Errorf("workload %s still exists after %s waiting for its deletion", name, deleteWaitTimeout)
			}
			return fmt.Errorf("waiting for workload %s to be deleted: %w", name, err)
		}
	}
}
