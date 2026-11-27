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
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// ledgerLines reads the ledger as raw JSON objects, one per line.
func ledgerLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("ledger line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestLedger_AppendsOneRecordPerItem(t *testing.T) {
	env := newLoopEnv(t)
	env.effects.outcomes = map[int32][]string{52: {"done"}, 53: {"park"}}
	items := env.queue(t, QueueItem{Issue: 52, Lane: "cpu"}, QueueItem{Issue: 53, Lane: "gpu"})
	env.run(t, items)

	lines := ledgerLines(t, env.cfg.LedgerPath)
	if len(lines) != 2 {
		t.Fatalf("ledger has %d records, want one per item:\n%v", len(lines), lines)
	}
	wantKeys := []string{"attempts", "ended", "finalStage", "issue", "lane", "parkKind", "prURL", "reason",
		"repo", "started", "workload"}
	byIssue := map[int]map[string]any{}
	for _, l := range lines {
		var keys []string
		for k := range l {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("record keys = %v, want %v", keys, wantKeys)
		}
		byIssue[int(l["issue"].(float64))] = l
	}
	done, parked := byIssue[52], byIssue[53]
	if done["finalStage"] != "done" || done["lane"] != "cpu" || done["workload"] != "wl-52" ||
		done["attempts"].(float64) != 1 || done["prURL"] != "https://example.test/pr/52" {
		t.Errorf("record for 52 = %v", done)
	}
	if parked["finalStage"] != "parked" || parked["parkKind"] != ParkEscalate || parked["reason"] != "stalled" ||
		parked["lane"] != "gpu" {
		t.Errorf("record for 53 = %v", parked)
	}
	for _, l := range lines {
		started, err1 := time.Parse(time.RFC3339Nano, l["started"].(string))
		ended, err2 := time.Parse(time.RFC3339Nano, l["ended"].(string))
		if err1 != nil || err2 != nil || started.IsZero() || ended.Before(started) {
			t.Errorf("record %v: started %v ended %v", l, started, ended)
		}
	}
}

func TestReadLedger_LastRecordPerIssue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := NewLedger(p)
	for _, r := range []LedgerRecord{
		{Issue: 52, FinalStage: ledgerParked},
		{Issue: 52, FinalStage: ledgerDone},
		{Issue: 53, FinalStage: ledgerDone},
		{Issue: 53, FinalStage: ledgerError},
		{Issue: 54, FinalStage: ledgerDropped},
		{Issue: 55, FinalStage: ledgerParked},
	} {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	last, err := ReadLedger(p, "")
	if err != nil {
		t.Fatal(err)
	}
	var finished []int32
	for n, r := range last {
		if ledgerFinished(r) {
			finished = append(finished, n)
		}
	}
	slices.Sort(finished)
	if !reflect.DeepEqual(finished, []int32{52, 54}) {
		t.Fatalf("finished = %v, want [52 54]", finished)
	}
}

func TestReadLedger_MissingIsEmptyAndBadLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	last, err := ReadLedger(filepath.Join(dir, "none.jsonl"), "")
	if err != nil || len(last) != 0 {
		t.Fatalf("missing ledger = %v, %v; want empty and no error", last, err)
	}
	p := filepath.Join(dir, "ledger.jsonl")
	if err := os.WriteFile(p, []byte(`{"issue":52,"finalStage":"done"}`+"\n"+`{"issue":53,`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLedger(p, ""); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("ReadLedger = %v, want an error naming line 2", err)
	}
}

// Decision 5: a finished item is not driven again after a restart, so verify
// and finalize do not run twice.
func TestRun_LedgerFinishedItemsAreNotDrivenAgain(t *testing.T) {
	env := newLoopEnv(t)
	l := NewLedger(env.cfg.LedgerPath)
	for _, r := range []LedgerRecord{
		{Issue: 52, FinalStage: ledgerDone},
		{Issue: 54, FinalStage: ledgerDropped},
		{Issue: 55, FinalStage: ledgerError},
	} {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	items := env.queue(t, QueueItem{Issue: 52}, QueueItem{Issue: 53}, QueueItem{Issue: 54},
		QueueItem{Issue: 55}, QueueItem{Issue: 56, After: []int32{52}})
	env.run(t, items)
	if got := env.effects.preflighted(); !reflect.DeepEqual(got, []int32{53, 55, 56}) {
		t.Fatalf("driven = %v, want [53 55 56]: 52 and 54 are finished, and 56 runs after a finished 52", got)
	}
}
