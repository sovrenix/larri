// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package state is LARRI's durable memory, and the reason a crash cannot lose
// an instance.
//
// Two artefacts with different jobs (§11.2):
//
//   - the **journal** is append-only and records what was *attempted*. It is
//     the authority, because an intent that never completed exists nowhere
//     else.
//   - the **snapshot** is a convenience view of current state, written
//     atomically so a crash mid-write leaves the previous valid one intact.
//
// When they disagree the journal wins. That is not a tie-break rule, it is the
// point: the case where they disagree is exactly the case where something was
// spent and not recorded anywhere else.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// Store is the on-disk state directory.
type Store struct {
	dir     string
	journal *Journal
	now     func() time.Time
}

// Open prepares a state directory, creating it if needed.
//
// Permissions are 0700 on directories and 0600 on files throughout. The
// directory holds rig tokens and ephemeral SSH keys, and loopback is not a
// per-user boundary, so a shared machine's other accounts must not be able to
// read it (FR-SEC-11).
func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "rigs"), filepath.Join(dir, "backups")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("state: create %s: %w", d, err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("state: chmod %s: %w", d, err)
		}
	}
	j, err := openJournal(JournalPath(dir))
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, journal: j, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Close releases the journal handle.
func (s *Store) Close() error { return s.journal.Close() }

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// SetClock replaces the time source, so lifecycles are testable without
// sleeping.
func (s *Store) SetClock(f func() time.Time) { s.now = f }

func (s *Store) rigPath(id string) string {
	return filepath.Join(s.dir, "rigs", id+".json")
}

// RecordIntent journals a transition **before** the action it describes.
//
// This is FR-PROV-01 and it is the single most important call in the package.
// The create call that follows may time out, may succeed without answering, or
// may kill the process midway; in every one of those cases the journal already
// names the rig, the provider, and the offer, so reconciliation can find what
// was created by its label. An intent written afterwards would record only the
// spends that went well.
func (s *Store) RecordIntent(rig *core.Rig, to core.LifecycleState, note string) error {
	e := Entry{
		At:      s.now(),
		RigID:   rig.ID,
		From:    rig.State,
		To:      to,
		Note:    note,
		PriceHr: rig.Offer.PriceHr,
	}
	if rig.Offer.Provider != "" {
		e.Provider, e.Offer = rig.Offer.Provider, rig.Offer.OfferID
	}
	if rig.Instance != nil {
		e.Instance = rig.Instance.InstanceID
		e.StorageHr = rig.Instance.StorageHr
	}
	return s.journal.Append(e)
}

// Transition journals a completed move and updates the snapshot.
func (s *Store) Transition(rig *core.Rig, to core.LifecycleState, note string) error {
	if err := s.RecordIntent(rig, to, note); err != nil {
		return err
	}
	rig.History = append(rig.History, core.Transition{
		At: s.now(), From: rig.State, To: to, Note: note,
	})
	rig.State = to
	return s.Save(rig)
}

// Save writes a rig snapshot atomically: temp file, fsync, rename, fsync the
// directory.
//
// The rename is atomic on POSIX, so a crash at any point leaves either the
// previous complete snapshot or the new complete one — never a half-written
// file that parses into a rig with no instance ID (FR-STATE-02).
func (s *Store) Save(rig *core.Rig) error {
	if !ValidID(rig.ID) {
		return fmt.Errorf("state: malformed rig id %q", rig.ID)
	}
	b, err := json.MarshalIndent(rig, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal rig %s: %w", rig.ID, err)
	}
	b = append(b, '\n')

	final := s.rigPath(rig.ID)
	tmp, err := os.CreateTemp(filepath.Dir(final), "."+rig.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp for %s: %w", rig.ID, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("state: chmod temp for %s: %w", rig.ID, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("state: write temp for %s: %w", rig.ID, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("state: sync temp for %s: %w", rig.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp for %s: %w", rig.ID, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("state: rename snapshot for %s: %w", rig.ID, err)
	}
	// Fsync the directory so the rename itself survives power loss, not just
	// the file contents it points at.
	return syncDir(filepath.Dir(final))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("state: open dir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("state: sync dir %s: %w", dir, err)
	}
	return nil
}

// Load reads one rig snapshot.
func (s *Store) Load(id string) (*core.Rig, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("state: malformed rig id %q", id)
	}
	b, err := os.ReadFile(s.rigPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: read rig %s: %w", id, err)
	}
	var rig core.Rig
	if err := json.Unmarshal(b, &rig); err != nil {
		return nil, fmt.Errorf("state: parse rig %s: %w", id, err)
	}
	return &rig, nil
}

// List returns every rig snapshot, newest first.
func (s *Store) List() ([]*core.Rig, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "rigs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: list rigs: %w", err)
	}
	var ids []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		id := n[:len(n)-len(".json")]
		if ValidID(id) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids))) // IDs sort by time
	out := make([]*core.Rig, 0, len(ids))
	for _, id := range ids {
		r, err := s.Load(id)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// Entries returns the whole journal.
func (s *Store) Entries() ([]Entry, error) { return ReadJournal(JournalPath(s.dir)) }

// Billable returns rigs that local state believes are costing money.
//
// Derived from the journal rather than from snapshots, because the case that
// matters — a create whose response never landed — has a journal entry and no
// snapshot at all.
func (s *Store) Billable() ([]string, error) {
	entries, err := s.Entries()
	if err != nil {
		return nil, err
	}
	last := map[string]core.LifecycleState{}
	var order []string
	for _, e := range entries {
		if _, seen := last[e.RigID]; !seen {
			order = append(order, e.RigID)
		}
		last[e.RigID] = e.To
	}
	var out []string
	for _, id := range order {
		if last[id].Billable() {
			out = append(out, id)
		}
	}
	return out, nil
}
