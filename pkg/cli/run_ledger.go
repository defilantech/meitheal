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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The finalStage values a ledger record carries.
const (
	// ledgerDone: finalized, skipped at preflight, or hand-fixed.
	ledgerDone = "done"
	// ledgerParked: a decision was parked for a human.
	ledgerParked = "parked"
	// ledgerDropped: the maintainer answered drop.
	ledgerDropped = "dropped"
	// ledgerError: the drive stopped on an error; a restart retries it.
	ledgerError = "error"
)

// LedgerRecord is one line of the run ledger: how one drive of one item, or
// one answer that finished it, ended. Every key is always written, so a
// reader can rely on the shape.
type LedgerRecord struct {
	Issue      int32     `json:"issue"`
	Lane       string    `json:"lane"`
	Workload   string    `json:"workload"`
	Attempts   int       `json:"attempts"`
	FinalStage string    `json:"finalStage"`
	ParkKind   string    `json:"parkKind"`
	Reason     string    `json:"reason"`
	PRURL      string    `json:"prURL"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended"`
}

// ledgerFinished reports whether a record ends the item for good: a run
// that reads it at start does not drive the item again, so verify and
// finalize never run twice for one issue.
func ledgerFinished(r LedgerRecord) bool {
	return r.FinalStage == ledgerDone || r.FinalStage == ledgerDropped
}

// Ledger appends records to a JSONL file. Safe for concurrent use.
type Ledger struct {
	path string
	mu   sync.Mutex
}

// NewLedger returns a Ledger writing to path.
func NewLedger(path string) *Ledger { return &Ledger{path: path} }

// Append writes r as one line. The line goes out in a single write on a file
// opened for appending, so a reader never sees half a record from a
// healthy write.
func (l *Ledger) Append(r LedgerRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	//nolint:gosec // G304: operator-supplied path
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("ledger: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("ledger: %w", err)
	}
	return f.Close()
}

// ReadLedger returns each issue's last record. A missing file is an empty
// ledger. A line that does not parse is an error naming it: the ledger is
// what keeps a finished item from being driven again, and guessing past a
// damaged line could re-run a finalize.
func ReadLedger(path string) (map[int32]LedgerRecord, error) {
	out := map[int32]LedgerRecord{}
	b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r LedgerRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("ledger %s line %d does not parse: %w", path, n, err)
		}
		out[r.Issue] = r
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	return out, nil
}
