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
	"slices"
	"testing"
)

func TestNextStage(t *testing.T) {
	cases := []struct {
		name     string
		cur      Stage
		facts    Facts
		wantNext Stage
		wantPark string
	}{
		{"preflight skip ends the item", StagePreflight,
			Facts{SkipReason: "open PR #1234 references it"}, StageDone, ""},
		{"preflight clean dispatches", StagePreflight, Facts{}, StageDispatch, ""},
		{"dispatch always watches", StageDispatch, Facts{}, StageWatch, ""},
		{"a stalled run parks for escalation", StageWatch,
			Facts{Stalled: true}, StageParked, "escalate"},
		{"a terminal run verifies", StageWatch, Facts{}, StageVerify, ""},
		{"clean verify on the first attempt finalizes", StageVerify,
			Facts{VerifyClean: true, Attempts: 1}, StageFinalize, ""},
		{"dirty verify on the first attempt goes to feedback", StageVerify,
			Facts{VerifyClean: false, Attempts: 1}, StageFeedback, ""},
		{"dirty verify after the feedback pass escalates", StageVerify,
			Facts{VerifyClean: false, Attempts: 2}, StageParked, "escalate"},
		{"feedback goes back to watch", StageFeedback, Facts{Attempts: 2}, StageWatch, ""},
		{"a concern parks needs-decision", StageWatch,
			Facts{Concern: "the issue contradicts the spec"}, StageParked, "needs-decision"},
		{"a failed workload parks escalate", StageWatch,
			Facts{WorkloadFailed: true}, StageParked, "escalate"},
		{"clean verify after the one feedback pass finalizes", StageVerify,
			Facts{VerifyClean: true, Attempts: 2}, StageFinalize, ""},
		{"finalize ends the item", StageFinalize, Facts{}, StageDone, ""},
		{"a busy GPU parks escalate on the first attempt, before feedback", StageVerify,
			Facts{GPUBusy: true, Attempts: 1}, StageParked, "escalate"},
		{"a busy GPU parks even when the rest says clean", StageVerify,
			Facts{GPUBusy: true, VerifyClean: true, Attempts: 1}, StageParked, "escalate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NextStage(tc.cur, tc.facts)
			if got.Next != tc.wantNext {
				t.Errorf("Next = %q, want %q", got.Next, tc.wantNext)
			}
			if got.Park != tc.wantPark {
				t.Errorf("Park = %q, want %q", got.Park, tc.wantPark)
			}
		})
	}
}

// The one-pass rule is the invariant, not a convention: after the single
// feedback attempt a dirty verify must escalate, never revise again.
func TestNextStage_NeverMakesASecondFeedbackAttempt(t *testing.T) {
	got := NextStage(StageVerify, Facts{VerifyClean: false, Attempts: 2})
	if got.Next != StageParked || got.Park != "escalate" {
		t.Fatalf("second dirty verify = (%q,%q), want (parked,escalate)", got.Next, got.Park)
	}
	// And no attempt count beyond that must always escalate, never adjudicate.
	for n := 3; n < 10; n++ {
		g := NextStage(StageVerify, Facts{VerifyClean: false, Attempts: n})
		if g.Next != StageParked || g.Park != "escalate" {
			t.Fatalf("attempts=%d = (%q,%q), want (parked,escalate)", n, g.Next, g.Park)
		}
	}
}

func TestNextStage_WatchParkReasons(t *testing.T) {
	got := NextStage(StageWatch, Facts{WorkloadFailed: true, ReviewRejected: true})
	if got.Park != ParkEscalate || got.Reason != "review did not converge" {
		t.Errorf("rejected review = (%q,%q), want (escalate, review did not converge)", got.Park, got.Reason)
	}
	// Any other failure does not claim a review verdict it never had.
	got = NextStage(StageWatch, Facts{WorkloadFailed: true})
	if got.Park != ParkEscalate || got.Reason != "the run failed before a review verdict" {
		t.Errorf("other failure = (%q,%q), want (escalate, the run failed before a review verdict)",
			got.Park, got.Reason)
	}
	// A stall is the more specific fact and keeps its own reason.
	got = NextStage(StageWatch, Facts{Stalled: true, WorkloadFailed: true, Concern: "x"})
	if got.Park != ParkEscalate || got.Reason != "stalled" {
		t.Errorf("stalled+failed+concern = (%q,%q), want (escalate,stalled)", got.Park, got.Reason)
	}
}

func TestOptionsFor_EveryParkOffersTheAnswerCommands(t *testing.T) {
	cases := map[string][]string{
		ParkEscalate:      {"requeue", "decide", "hand-fix", "drop"},
		ParkNeedsDecision: {"requeue", "decide", "drop"},
		ParkNeedsSplit:    {"requeue", "decide", "drop"},
	}
	for kind, want := range cases {
		if got := optionsFor(kind); !slices.Equal(got, want) {
			t.Errorf("optionsFor(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestNextStage_GPUBusyReason(t *testing.T) {
	if got := NextStage(StageVerify, Facts{GPUBusy: true, Attempts: 1}); got.Reason != "GPU node stayed busy" {
		t.Fatalf("Reason = %q", got.Reason)
	}
}

// A lost run parks escalate at preflight, ahead of any skip reason.
func TestNextStage_RunLostParksEscalate(t *testing.T) {
	got := NextStage(StagePreflight, Facts{RunLost: true, SkipReason: "x"})
	if got.Next != StageParked || got.Park != ParkEscalate || got.Reason != RunLostReason {
		t.Fatalf("run lost = %+v, want parked escalate %q", got, RunLostReason)
	}
}
