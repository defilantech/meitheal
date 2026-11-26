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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// laneWatch records how many drives run at once, per lane and overall.
type laneWatch struct {
	mu      sync.Mutex
	now     map[string]int
	max     map[string]int
	all     int
	maxAll  int
	events  []string
	started []int32
}

func newLaneWatch() *laneWatch {
	return &laneWatch{now: map[string]int{}, max: map[string]int{}}
}

func (w *laneWatch) enter(it QueueItem) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now[it.Lane]++
	w.all++
	w.max[it.Lane] = max(w.max[it.Lane], w.now[it.Lane])
	w.maxAll = max(w.maxAll, w.all)
	w.events = append(w.events, fmt.Sprintf("start %d", it.Issue))
	w.started = append(w.started, it.Issue)
}

func (w *laneWatch) leave(it QueueItem) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now[it.Lane]--
	w.all--
	w.events = append(w.events, fmt.Sprintf("end %d", it.Issue))
}

// runLanesWithin fails the test instead of hanging when RunLanes deadlocks.
func runLanesWithin(t *testing.T, ctx context.Context, items []QueueItem, limits map[string]int,
	drive func(context.Context, QueueItem) (Stage, error)) map[int32]Stage {
	t.Helper()
	done := make(chan map[int32]Stage, 1)
	go func() { done <- RunLanes(ctx, items, limits, drive) }()
	select {
	case got := <-done:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("RunLanes did not return: a deadlock")
		return nil
	}
}

func TestRunLanes_RespectsLimits(t *testing.T) {
	items := []QueueItem{
		{Issue: 1, Lane: "cpu"}, {Issue: 2, Lane: "cpu"}, {Issue: 3, Lane: "cpu"},
		{Issue: 4, Lane: "gpu"}, {Issue: 5, Lane: "gpu"},
	}
	w := newLaneWatch()
	got := runLanesWithin(t, context.Background(), items, map[string]int{"cpu": 1, "gpu": 1},
		func(_ context.Context, it QueueItem) (Stage, error) {
			w.enter(it)
			time.Sleep(15 * time.Millisecond)
			w.leave(it)
			return StageDone, nil
		})
	if w.max["cpu"] != 1 || w.max["gpu"] != 1 {
		t.Fatalf("most at once per lane = %v, want 1 on each", w.max)
	}
	// The lanes are independent: a cpu item and a gpu item run side by side.
	if w.maxAll != 2 {
		t.Fatalf("most at once overall = %d, want 2 (one per lane)", w.maxAll)
	}
	want := map[int32]Stage{1: StageDone, 2: StageDone, 3: StageDone, 4: StageDone, 5: StageDone}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunLanes = %v, want %v", got, want)
	}
	// Queue order within a lane.
	var cpu []int32
	for _, n := range w.started {
		if n <= 3 {
			cpu = append(cpu, n)
		}
	}
	if !reflect.DeepEqual(cpu, []int32{1, 2, 3}) {
		t.Fatalf("cpu lane started %v, want queue order [1 2 3]", cpu)
	}
}

func TestRunLanes_HigherLimitRunsSideBySide(t *testing.T) {
	items := []QueueItem{{Issue: 1, Lane: "cpu"}, {Issue: 2, Lane: "cpu"}, {Issue: 3, Lane: "cpu"}}
	w := newLaneWatch()
	runLanesWithin(t, context.Background(), items, map[string]int{"cpu": 2, "gpu": 1},
		func(_ context.Context, it QueueItem) (Stage, error) {
			w.enter(it)
			time.Sleep(15 * time.Millisecond)
			w.leave(it)
			return StageDone, nil
		})
	if w.max["cpu"] != 2 {
		t.Fatalf("most cpu at once = %d, want 2", w.max["cpu"])
	}
}

func TestRunLanes_AfterOrdering(t *testing.T) {
	// Different lanes, so only After keeps 53 from starting with 52.
	items := []QueueItem{{Issue: 52, Lane: "cpu"}, {Issue: 53, Lane: "gpu", After: []int32{52}}}
	w := newLaneWatch()
	got := runLanesWithin(t, context.Background(), items, map[string]int{"cpu": 1, "gpu": 1},
		func(_ context.Context, it QueueItem) (Stage, error) {
			w.enter(it)
			if it.Issue == 52 {
				time.Sleep(20 * time.Millisecond)
			}
			w.leave(it)
			return StageDone, nil
		})
	want := []string{"start 52", "end 52", "start 53", "end 53"}
	if !reflect.DeepEqual(w.events, want) {
		t.Fatalf("events = %v, want %v", w.events, want)
	}
	if got[53] != StageDone {
		t.Fatalf("RunLanes = %v, want 53 done", got)
	}
}

// Decision 6: a dependency that stopped without finishing (parked here)
// does not release what waits on it, and RunLanes still returns.
func TestRunLanes_ParkedDependencyHoldsItsDependents(t *testing.T) {
	items := []QueueItem{
		{Issue: 52, Lane: "cpu"},
		{Issue: 53, Lane: "gpu", After: []int32{52}},
		{Issue: 54, Lane: "cpu", After: []int32{53}},
		{Issue: 55, Lane: "cpu"},
	}
	w := newLaneWatch()
	got := runLanesWithin(t, context.Background(), items, map[string]int{"cpu": 1, "gpu": 1},
		func(_ context.Context, it QueueItem) (Stage, error) {
			w.enter(it)
			defer w.leave(it)
			if it.Issue == 52 {
				return StageParked, nil
			}
			return StageDone, nil
		})
	want := map[int32]Stage{52: StageParked, 55: StageDone}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunLanes = %v, want %v (53 and 54 never started)", got, want)
	}
	for _, n := range w.started {
		if n == 53 || n == 54 {
			t.Fatalf("issue %d started although what it runs after never finished", n)
		}
	}
}

func TestRunLanes_FailedDependencyHoldsItsDependents(t *testing.T) {
	items := []QueueItem{{Issue: 52, Lane: "cpu"}, {Issue: 53, Lane: "cpu", After: []int32{52}}}
	got := runLanesWithin(t, context.Background(), items, map[string]int{"cpu": 1},
		func(_ context.Context, it QueueItem) (Stage, error) {
			if it.Issue == 52 {
				return StageWatch, fmt.Errorf("forge unreachable")
			}
			return StageDone, nil
		})
	if _, ok := got[53]; ok || got[52] != StageWatch {
		t.Fatalf("RunLanes = %v, want 52 stopped at watch and 53 never started", got)
	}
}

// RunLanes checks its input itself: a queue CheckQueueOrder refuses drives
// nothing.
func TestRunLanes_RefusesAQueueItCannotSchedule(t *testing.T) {
	for name, items := range map[string][]QueueItem{
		"unknown dependency": {{Issue: 53, Lane: "cpu", After: []int32{52}}},
		"cycle":              {{Issue: 1, After: []int32{2}}, {Issue: 2, After: []int32{1}}},
		"duplicate":          {{Issue: 1}, {Issue: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			got := runLanesWithin(t, context.Background(), items, map[string]int{"cpu": 1},
				func(context.Context, QueueItem) (Stage, error) { calls++; return StageDone, nil })
			if calls != 0 || len(got) != 0 {
				t.Fatalf("drives = %d, result %v; want nothing driven", calls, got)
			}
		})
	}
}

func TestRunLanes_CancelledContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int
	got := runLanesWithin(t, ctx, []QueueItem{{Issue: 1, Lane: "cpu"}}, map[string]int{"cpu": 1},
		func(context.Context, QueueItem) (Stage, error) { calls++; return StageDone, nil })
	if calls != 0 || len(got) != 0 {
		t.Fatalf("drives = %d, result %v; want nothing started on a cancelled context", calls, got)
	}
}

func TestCheckQueueOrder(t *testing.T) {
	ok := [][]QueueItem{
		{{Issue: 1}, {Issue: 2, After: []int32{1}}, {Issue: 3, After: []int32{1, 2}}},
		{{Issue: 3, After: []int32{1}}, {Issue: 1}},
	}
	for i, items := range ok {
		if err := CheckQueueOrder(items); err != nil {
			t.Errorf("case %d: CheckQueueOrder = %v, want nil", i, err)
		}
	}
	bad := []struct {
		name  string
		items []QueueItem
		want  []string
	}{
		{"duplicate", []QueueItem{{Issue: 1}, {Issue: 2}, {Issue: 1}}, []string{"issue 1", "twice"}},
		{"two-cycle", []QueueItem{{Issue: 1, After: []int32{2}}, {Issue: 2, After: []int32{1}}},
			[]string{"cycle", "1", "2"}},
		{"three-cycle", []QueueItem{
			{Issue: 1, After: []int32{3}}, {Issue: 2, After: []int32{1}}, {Issue: 3, After: []int32{2}},
		}, []string{"cycle"}},
		{"self", []QueueItem{{Issue: 1, After: []int32{1}}}, []string{"cycle"}},
		{"unknown", []QueueItem{{Issue: 1, After: []int32{9}}}, []string{"issue 9", "not in the queue"}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckQueueOrder(tc.items)
			if err == nil {
				t.Fatal("CheckQueueOrder = nil, want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to contain %q", err, w)
				}
			}
		})
	}
}

func TestParseParallel(t *testing.T) {
	ok := map[string]map[string]int{
		"cpu=1,gpu=1":     {"cpu": 1, "gpu": 1},
		"cpu=3,gpu=2":     {"cpu": 3, "gpu": 2},
		" gpu=2 , cpu=1 ": {"cpu": 1, "gpu": 2},
		"cpu=2":           {"cpu": 2, "gpu": 1},
	}
	for in, want := range ok {
		got, err := ParseParallel(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseParallel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "tpu=1", "cpu=0", "cpu=-1", "cpu", "cpu=x", "cpu=1,cpu=2", "cpu=1,,gpu=1"} {
		if got, err := ParseParallel(in); err == nil {
			t.Errorf("ParseParallel(%q) = %v, want an error", in, got)
		}
	}
}
