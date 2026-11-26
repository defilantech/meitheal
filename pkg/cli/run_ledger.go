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
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The finalStage values a ledger record carries.
const (
	// ledgerDone: finalized, or hand-fixed.
	ledgerDone = "done"
	// ledgerSkipped: preflight found the work in flight elsewhere (an open
	// PR, a pushed branch). Not finished: a restart runs preflight again,
	// and what runs after the item keeps waiting.
	ledgerSkipped = "skipped"
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
// healthy write. A crashed append leaves a last line with no newline; that
// partial record never finished, so it is cut off before the next one is
// written rather than left to make the line after it unreadable.
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
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	if err := cutTornTail(f); err != nil {
		_ = f.Close()
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

// cutTornTail truncates the file after its last newline when its last byte
// is not one: what follows is an append that did not finish.
func cutTornTail(f *os.File) error {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil && err != io.EOF {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	b := make([]byte, fi.Size())
	if _, err := f.ReadAt(b, 0); err != nil && err != io.EOF {
		return err
	}
	return f.Truncate(int64(bytes.LastIndexByte(b, '\n') + 1))
}

// ReadLedger returns each issue's last record. A missing file is an empty
// ledger. A line that does not parse is an error naming it: the ledger is
// what keeps a finished item from being driven again, and guessing past a
// damaged line could re-run a finalize. The one exception is a last line
// with no newline, which is what a crash in the middle of an append leaves:
// it is ignored and logged.
func ReadLedger(path string) (map[int32]LedgerRecord, error) {
	return readLedger(path, log.Printf)
}

func readLedger(path string, logf func(string, ...any)) (map[int32]LedgerRecord, error) {
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
	torn := len(b) > 0 && b[len(b)-1] != '\n'
	lines := bytes.Count(b, []byte{'\n'})
	if torn {
		lines++
	}
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r LedgerRecord
		if err := json.Unmarshal(line, &r); err != nil {
			if torn && n == lines {
				logf("ledger %s: ignoring line %d, an append that did not finish", path, n)
				continue
			}
			return nil, fmt.Errorf("ledger %s line %d does not parse: %w", path, n, err)
		}
		out[r.Issue] = r
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	return out, nil
}
