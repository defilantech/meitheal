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
	"strings"
)

// The lanes a queue item can run in. ParseQueue fills in laneCPU when an
// item names none.
const (
	laneCPU = "cpu"
	laneGPU = "gpu"
)

// ParseParallel parses --parallel: "cpu=N,gpu=M", each N at least 1. A lane
// left out keeps its default of 1; an unknown lane, a repeated lane or a
// limit below 1 is an error, since a lane limited to 0 would hold its items
// forever.
func ParseParallel(s string) (map[string]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("want cpu=N,gpu=M")
	}
	out := map[string]int{laneCPU: 1, laneGPU: 1}
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		lane, num, ok := strings.Cut(strings.TrimSpace(part), "=")
		lane = strings.TrimSpace(lane)
		if !ok {
			return nil, fmt.Errorf("%q is not lane=N", strings.TrimSpace(part))
		}
		if lane != laneCPU && lane != laneGPU {
			return nil, fmt.Errorf("unknown lane %q, want cpu or gpu", lane)
		}
		if seen[lane] {
			return nil, fmt.Errorf("lane %s is given twice", lane)
		}
		seen[lane] = true
		n, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("lane %s: %q is not a whole number of at least 1", lane, num)
		}
		out[lane] = n
	}
	return out, nil
}

// CheckQueueOrder refuses a queue the lanes cannot schedule: an issue listed
// twice, an After naming an issue that is not in the queue, or After
// dependencies that form a cycle. ParseQueue catches a self-reference and an
// unknown issue but not a cycle, and a cycle would leave every item on it
// waiting for the others forever.
func CheckQueueOrder(items []QueueItem) error {
	byIssue := map[int32]QueueItem{}
	for _, it := range items {
		if _, dup := byIssue[it.Issue]; dup {
			return fmt.Errorf("queue: issue %d is listed twice", it.Issue)
		}
		byIssue[it.Issue] = it
	}
	for _, it := range items {
		for _, a := range it.After {
			if _, ok := byIssue[a]; !ok {
				return fmt.Errorf("queue: issue %d runs after issue %d, which is not in the queue", it.Issue, a)
			}
		}
	}
	const (
		unvisited = iota
		visiting
		visited
	)
	color := map[int32]int{}
	var path []int32
	var visit func(n int32) error
	visit = func(n int32) error {
		switch color[n] {
		case visited:
			return nil
		case visiting:
			start := 0
			for i, p := range path {
				if p == n {
					start = i
				}
			}
			cycle := make([]string, 0, len(path)-start+1)
			for _, p := range path[start:] {
				cycle = append(cycle, strconv.Itoa(int(p)))
			}
			cycle = append(cycle, strconv.Itoa(int(n)))
			return fmt.Errorf("queue: the after lists form a cycle (%s), so none of those issues could ever start",
				strings.Join(cycle, " -> "))
		}
		color[n] = visiting
		path = append(path, n)
		for _, a := range byIssue[n].After {
			if err := visit(a); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		color[n] = visited
		return nil
	}
	for _, it := range items {
		if err := visit(it.Issue); err != nil {
			return err
		}
	}
	return nil
}

// laneOf is an item's lane, cpu when it names none.
func laneOf(it QueueItem) string {
	if it.Lane == "" {
		return laneCPU
	}
	return it.Lane
}

type laneResult struct {
	item  QueueItem
	stage Stage
	err   error
}

// laneScheduler starts drives within per-lane limits and reports each one's
// end on results. It is owned by one goroutine (the caller of startReady and
// finish); only the drives themselves run elsewhere.
type laneScheduler struct {
	limits   map[string]int
	drive    func(context.Context, QueueItem) (Stage, error)
	running  map[string]int
	inflight map[int32]bool
	results  chan laneResult
}

func newLaneScheduler(limits map[string]int, drive func(context.Context, QueueItem) (Stage, error)) *laneScheduler {
	return &laneScheduler{
		limits: limits, drive: drive,
		running: map[string]int{}, inflight: map[int32]bool{},
		results: make(chan laneResult),
	}
}

// limit is a lane's limit. A lane with no limit, or a limit below 1, runs one
// at a time: a zero would hold its items forever.
func (s *laneScheduler) limit(lane string) int {
	if n := s.limits[lane]; n >= 1 {
		return n
	}
	return 1
}

func (s *laneScheduler) busy() int { return len(s.inflight) }

func (s *laneScheduler) has(issue int32) bool { return s.inflight[issue] }

// startReady starts, in order, every pending item whose lane has room and for
// which ready holds, and returns the ones it did not start. Nothing starts
// on a cancelled context.
func (s *laneScheduler) startReady(ctx context.Context, pending []QueueItem, ready func(QueueItem) bool) []QueueItem {
	if ctx.Err() != nil {
		return pending
	}
	var rest []QueueItem
	for _, it := range pending {
		lane := laneOf(it)
		if s.inflight[it.Issue] || s.running[lane] >= s.limit(lane) || !ready(it) {
			rest = append(rest, it)
			continue
		}
		s.running[lane]++
		s.inflight[it.Issue] = true
		go func(it QueueItem) {
			st, err := s.drive(ctx, it)
			s.results <- laneResult{item: it, stage: st, err: err}
		}(it)
	}
	return rest
}

// finish releases the lane slot of a drive that reported on results.
func (s *laneScheduler) finish(r laneResult) {
	s.running[laneOf(r.item)]--
	delete(s.inflight, r.item.Issue)
}

// RunLanes drives items with at most limits[lane] of each lane at once, in
// queue order within a lane, and returns the stage each driven item ended at.
//
// An item starts only once every item it runs After has ended StageDone
// without an error. A dependency that parked or failed holds its dependents:
// they are never started and are absent from the result, because stopping
// is not finishing. A dependency that is not among items is the caller's to
// have settled and does not hold anything. RunLanes returns when nothing is
// running and nothing more can start, so it cannot deadlock, cycle or not
// (CheckQueueOrder refuses cycles before a run begins).
//
// A cancelled context starts nothing more; RunLanes waits for the drives
// already running, which see the same context, to return.
func RunLanes(
	ctx context.Context, items []QueueItem, limits map[string]int,
	drive func(context.Context, QueueItem) (Stage, error),
) map[int32]Stage {
	inBatch := map[int32]bool{}
	for _, it := range items {
		inBatch[it.Issue] = true
	}
	out := map[int32]Stage{}
	finished := map[int32]bool{}
	ready := func(it QueueItem) bool {
		for _, a := range it.After {
			if inBatch[a] && !finished[a] {
				return false
			}
		}
		return true
	}
	s := newLaneScheduler(limits, drive)
	pending := append([]QueueItem(nil), items...)
	for {
		pending = s.startReady(ctx, pending, ready)
		if s.busy() == 0 {
			return out
		}
		r := <-s.results
		s.finish(r)
		out[r.item.Issue] = r.stage
		finished[r.item.Issue] = r.err == nil && r.stage == StageDone
	}
}
