// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// Entry is one line of the journal.
//
// The journal records **what was attempted**, which is a different question
// from what the current state is. A snapshot can only describe an action that
// finished; an intent that never completed exists nowhere else. That is why
// the journal is the authority when the two disagree (§11.2).
type Entry struct {
	At       time.Time           `json:"ts"`
	RigID    string              `json:"rig"`
	From     core.LifecycleState `json:"from"`
	To       core.LifecycleState `json:"to"`
	Note     string              `json:"note,omitempty"`
	Provider string              `json:"provider,omitempty"`
	Offer    string              `json:"offer,omitempty"`
	Instance string              `json:"instance,omitempty"`
	PriceHr  float64             `json:"price_hr,omitempty"`

	// StorageHr is charged for as long as the resource exists, including
	// while STOPPED, so cost derived from this journal must account for it
	// separately from compute.
	StorageHr float64 `json:"storage_hr,omitempty"`

	// Trace correlation. Written from M1 even though the SDK is wired in M5:
	// the journal format is durable, and adding fields later means migrating
	// records that are already on disk (§20.2).
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`

	// Termination is set on the entry that ends a rig, so "why is my rig
	// gone" is answerable from the journal alone even if the snapshot was
	// pruned (§13.1).
	Termination *core.Termination `json:"termination,omitempty"`
}

// Journal is an append-only record. It is never rewritten, never compacted,
// and never truncated: the one file that must survive every failure mode is
// the one that must never be edited in place.
type Journal struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func openJournal(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: open journal: %w", err)
	}
	return &Journal{path: path, f: f}, nil
}

// Append writes one entry and flushes it to disk before returning.
//
// The fsync is not optional and is not a performance question. An entry that
// is only in the page cache when the machine loses power is an intent that was
// never recorded, and the whole point of the journal is that the record
// precedes the spend (FR-PROV-01). Transitions are low volume — a handful per
// rig lifetime — so the cost is irrelevant next to what it buys.
func (j *Journal) Append(e Entry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("state: marshal journal entry: %w", err)
	}
	b = append(b, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.f.Write(b); err != nil {
		return fmt.Errorf("state: append journal: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("state: sync journal: %w", err)
	}
	return nil
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// ReadJournal returns every entry, oldest first.
//
// A truncated final line — the signature of a crash mid-write — is dropped
// rather than treated as corruption. Losing the last intent is bad; refusing
// to read the ninety before it would be worse, because those are what the
// reconciler needs to find billable resources.
func ReadJournal(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: open journal: %w", err)
	}
	defer f.Close()

	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // truncated tail; see doc comment
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("state: read journal: %w", err)
	}
	return out, nil
}

// EntriesFor returns the entries belonging to one rig, oldest first.
func EntriesFor(entries []Entry, rigID string) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.RigID == rigID {
			out = append(out, e)
		}
	}
	return out
}

// JournalPath is where the journal lives inside a state directory.
func JournalPath(dir string) string { return filepath.Join(dir, "journal.jsonl") }
