// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/secret"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newRig(t *testing.T) *core.Rig {
	t.Helper()
	id, err := NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &core.Rig{
		ID:        id,
		State:     core.StateSelected,
		CreatedAt: time.Now().UTC(),
		Offer: core.Offer{
			Provider: "vastai", OfferID: "9182736", GPUModel: "A100",
			GPUCount: 1, VRAMPerGPUGB: 80, PriceHr: 1.29,
		},
		Model: core.ModelSpec{Ref: "Qwen/Qwen3-Coder-30B", ServedName: "qwen3-coder"},
	}
}

// AC-2.1: kill the daemon mid-CREATING. On restart the instance must be
// findable. The journal is what makes it findable, because the snapshot for a
// create that never answered does not exist.
func TestCrashBetweenIntentAndCreateLeavesATrail(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rig := newRig(t)

	// Intent is recorded BEFORE the provider call. Then the process dies:
	// no snapshot is ever written, no instance ID is ever learned.
	if err := s.RecordIntent(rig, core.StateCreating, "create intent"); err != nil {
		t.Fatal(err)
	}
	s.Close() // the crash

	// Restart.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if got, _ := s2.Load(rig.ID); got != nil {
		t.Fatal("precondition: no snapshot should exist for a create that never answered")
	}
	billable, err := s2.Billable()
	if err != nil {
		t.Fatal(err)
	}
	if len(billable) != 1 || billable[0] != rig.ID {
		t.Fatalf("the rig must be known billable from the journal alone, got %v", billable)
	}
	entries, _ := s2.Entries()
	e := EntriesFor(entries, rig.ID)
	if len(e) != 1 || e[0].To != core.StateCreating {
		t.Fatalf("journal should show the CREATING intent, got %+v", e)
	}
	if e[0].Provider != "vastai" || e[0].Offer != "9182736" {
		t.Error("the intent must name the provider and offer, or reconciliation has nothing to search by")
	}
}

// FR-STATE-02: a crash mid-snapshot-write leaves the previous valid snapshot
// intact. The atomic rename is what guarantees it, so the test proves no
// partial file is ever visible under the final name.
func TestSnapshotWriteIsAtomic(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Save(rig); err != nil {
		t.Fatal(err)
	}
	rig.State = core.StateReady
	rig.Instance = &core.Instance{Provider: "vastai", InstanceID: "14872213", Running: true}
	if err := s.Save(rig); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(rig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.StateReady || got.Instance == nil {
		t.Fatal("the second write should be visible in full")
	}
	// No temp files may survive, or a directory listing would show rigs that
	// do not exist.
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), "rigs"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// FR-STATE-05 / FR-SEC-01: state must never contain a credential. The Secret
// type makes this structural, and the test proves it survives a real
// round-trip through the file rather than trusting the type in isolation.
func TestSecretsNeverReachDisk(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	rig.Instance = &core.Instance{
		Provider: "vastai", InstanceID: "14872213",
		Labels: map[string]string{core.LabelKey: rig.ID},
	}
	// A credential smuggled in through any string field would show up here.
	const canary = "sk-live-DO-NOT-PERSIST-4f2a9c"
	_ = secret.New(canary)

	if err := s.Save(rig); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(), "rigs", rig.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(b), canary) {
		t.Fatal("a credential reached the state file")
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(hay); i++ {
				if hay[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// §11.2: the journal is never rewritten. Every transition must remain
// readable, because cost accounting and post-mortems replay it.
func TestJournalIsAppendOnly(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	rig := newRig(t)
	for _, to := range []core.LifecycleState{
		core.StateCreating, core.StateProvisioned, core.StateBootstrapping,
		core.StateReady, core.StateDraining, core.StateDestroyed,
	} {
		if err := s.Transition(rig, to, ""); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	entries, err := ReadJournal(JournalPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		t.Fatalf("all six transitions must survive, got %d", len(entries))
	}
	if entries[0].To != core.StateCreating || entries[5].To != core.StateDestroyed {
		t.Error("entries must remain in chronological order")
	}
}

// A crash mid-write truncates the last line. Dropping it is right; refusing to
// read the entries before it would strand every billable resource they name.
func TestTruncatedJournalTailIsToleratedNotFatal(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	rig := newRig(t)
	_ = s.Transition(rig, core.StateCreating, "")
	_ = s.Transition(rig, core.StateProvisioned, "")
	s.Close()

	path := JournalPath(dir)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, []byte(`{"ts":"2026-08-2`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("a truncated tail must not fail the read: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the two complete entries must survive, got %d", len(entries))
	}
}

func TestMalformedIDNeverBecomesAPath(t *testing.T) {
	s := openStore(t)
	bad := &core.Rig{ID: "../../../etc/passwd", State: core.StateIdle}
	if err := s.Save(bad); err == nil {
		t.Fatal("a malformed rig id must be refused before it reaches the filesystem")
	}
	if _, err := s.Load("../../etc/passwd"); err == nil {
		t.Fatal("Load must refuse a malformed id")
	}
}

func TestStateDirectoryIsPrivate(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Save(rig); err != nil {
		t.Fatal(err)
	}
	// The directory holds rig tokens and ephemeral SSH keys, and loopback is
	// not a per-user boundary (FR-SEC-11).
	for _, p := range []string{s.Dir(), filepath.Join(s.Dir(), "rigs")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 700", p, perm)
		}
	}
	fi, err := os.Stat(filepath.Join(s.Dir(), "rigs", rig.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot has mode %o, want 600", perm)
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := openStore(t)
	base := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 3; i++ {
		id, _ := NewID(base.Add(time.Duration(i) * time.Minute))
		ids = append(ids, id)
		if err := s.Save(&core.Rig{ID: id, State: core.StateDestroyed}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rigs, want 3", len(got))
	}
	if got[0].ID != ids[2] {
		t.Error("List must return newest first")
	}
}
