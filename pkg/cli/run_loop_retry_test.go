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
	"strings"
	"testing"
	"time"
)

// A drive that ends in an error is tried again on the next answer cycle,
// and a success ends the run of errors: nothing is parked.
func TestRun_TransientDriveErrorIsRetried(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"error", "done"}}
	items := env.queue(t, QueueItem{Issue: 52})

	env.run(t, items)

	led := env.ledger(t)[52]
	if len(led) != 2 || led[0].FinalStage != ledgerError || led[1].FinalStage != ledgerDone {
		t.Fatalf("ledger for 52 = %+v, want an error, then done", led)
	}
	if ds, _ := ListDecisions(env.cfg.DecisionsDir); len(ds) != 0 {
		t.Fatalf("decisions = %+v, want none after a transient error", ds)
	}
}

// The third error in a row parks the item escalate with a fixed reason, the
// park is announced and stamped like every other, and the item is not driven
// a fourth time.
func TestRun_ThreeDriveErrorsPark(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"error"}}
	items := env.queue(t, QueueItem{Issue: 52})

	env.run(t, items)

	if n := env.effects.drives[52]; n != maxDriveErrors {
		t.Fatalf("drives = %d, want %d", n, maxDriveErrors)
	}
	d, ok := env.decision(t, 52, ParkEscalate)
	if !ok || d.Reason != DriveErrorsReason || d.Repo != loopRepo || d.Opened.IsZero() {
		t.Fatalf("decision = %+v (found %v), want escalate %q", d, ok, DriveErrorsReason)
	}
	if !strings.Contains(d.Evidence["error"], "watch failed") {
		t.Errorf("evidence = %v, want the last error", d.Evidence)
	}
	if d.Notified == nil {
		t.Error("the park was not stamped as notified")
	}
	if got := env.effects.notifications(); len(got) != 1 || got[0].Reason != DriveErrorsReason {
		t.Fatalf("notified = %+v, want one notice of the park", got)
	}
	led := env.ledger(t)[52]
	if last := led[len(led)-1]; last.FinalStage != ledgerParked || last.Reason != DriveErrorsReason {
		t.Errorf("last ledger record = %+v, want the park", last)
	}
}

// A new round whose start fails keeps its marker: the retry starts the round
// again (Restart, not a resume through preflight), and the answer that
// started it is consumed only once the round has started.
func TestRun_RestartMarkerSurvivesAnErroredDrive(t *testing.T) {
	env := newLoopEnv(t)
	opened := env.park(t, 52, ParkEscalate, string(StageWatch), true)
	env.forge.answer(52, "/meitheal requeue", opened.Add(time.Minute))
	env.effects.outcomes = map[int32][]string{52: {"restartfail", "done"}}
	items := env.queue(t, QueueItem{Issue: 52})

	env.run(t, items)

	if got := env.effects.restarts(); !reflect.DeepEqual(got, []int32{52, 52}) {
		t.Fatalf("restarted = %v, want the new round started twice", got)
	}
	if got := env.effects.preflighted(); len(got) != 0 {
		t.Fatalf("preflighted = %v, want none: the retry is still the new round", got)
	}
	if _, ok := env.decision(t, 52, ParkEscalate); ok {
		t.Fatal("the answered decision is still there after the round started")
	}
	led := env.ledger(t)[52]
	if last := led[len(led)-1]; last.FinalStage != ledgerDone {
		t.Fatalf("ledger for 52 = %+v, want it done", led)
	}
}
