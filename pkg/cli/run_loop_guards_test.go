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
	"reflect"
	"testing"
)

// A closed issue is skipped at preflight and releases what runs after it.
func TestRun_ClosedIssueReleasesDependents(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"closed"}}
	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53, After: []int32{52}})

	env.run(t, items)

	led := env.ledger(t)
	if len(led[52]) != 1 || led[52][0].FinalStage != ledgerSkipped || led[52][0].Reason != IssueClosedReason {
		t.Fatalf("ledger for 52 = %+v, want skipped %q", led[52], IssueClosedReason)
	}
	if len(led[53]) != 1 || led[53][0].FinalStage != ledgerDone {
		t.Fatalf("ledger for 53 = %+v, want it driven to done after the closed 52", led[53])
	}
}

// Any other preflight skip still holds its dependents back.
func TestRun_OpenPRSkipStillHoldsDependents(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"skip"}}
	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53, After: []int32{52}})

	env.run(t, items)

	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{52}) {
		t.Fatalf("preflighted = %v, want only 52", got)
	}
}

// The issue's own branch with no Workload parks escalate and is announced,
// through the run loop as well as the driver.
func TestRun_LostRunParksAndNotifies(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"lost"}}
	items := env.queue(t, QueueItem{Issue: 52})

	env.run(t, items)

	d, ok := env.decision(t, 52, ParkEscalate)
	if !ok || d.Reason != RunLostReason || d.Notified == nil {
		t.Fatalf("decision = %+v (found %v), want a notified escalate %q", d, ok, RunLostReason)
	}
	if got := env.effects.notifications(); len(got) != 1 || got[0].Reason != RunLostReason {
		t.Fatalf("notified = %+v, want one notice", got)
	}
}
