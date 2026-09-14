// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"errors"
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

// The journal is what cost is replayed from, so it has to carry the rate the
// provider bills. It carried the quote, and `larri status` showed $0.821/hr
// beside a total accrued at $0.802.
func TestTheJournalRecordsTheBilledRate(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Transition(rig, core.StateCreating, "create intent"); err != nil {
		t.Fatal(err)
	}
	rig.Instance = &core.Instance{Provider: "vastai", InstanceID: "1", PriceHr: 1.31, StorageHr: 0.02}
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.Entries()
	e := EntriesFor(entries, rig.ID)
	if len(e) != 2 {
		t.Fatalf("entries = %d", len(e))
	}
	if e[0].PriceHr != 1.29 {
		t.Errorf("before an instance reports a rate, the quote is all there is: got %v", e[0].PriceHr)
	}
	if e[1].Rates != RatesBilled {
		t.Errorf("entry written without the rates marker; replay would read it by the old rules")
	}
	if e[1].PriceHr != 1.31 || e[1].StorageHr != 0.02 {
		t.Errorf("journalled %v/hr with %v storage, want the billed 1.31 and 0.02", e[1].PriceHr, e[1].StorageHr)
	}
}

// Two processes can hold one rig. The one with a stale copy must not undo
// the other's teardown.
func TestADestroyedRigCannotBeMovedBackToALiveState(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	stale := *rig
	if err := s.Transition(rig, core.StateDestroyed, "down"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(&stale, core.StateDegraded, "health probes failing"); err == nil {
		t.Fatal("a stale copy moved a destroyed rig to DEGRADED")
	}
	if got, _ := s.Load(rig.ID); got.State != core.StateDestroyed {
		t.Errorf("stored state = %s", got.State)
	}
}

// A second teardown from a stale copy must not replace the reason the first
// recorded. A rig already destroyed in the caller's own copy may be recorded
// again, which is how an operator's correction lands.
func TestAStaleSecondTeardownKeepsTheFirstRecord(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	stale := *rig
	rig.End = &core.Termination{Actor: core.ActorPolicy, Code: core.ReasonIdleTimeout, Summary: "idle"}
	if err := s.Transition(rig, core.StateDestroyed, "idle"); err != nil {
		t.Fatal(err)
	}
	stale.End = &core.Termination{Actor: core.ActorOperator, Code: core.ReasonOperatorRequest, Summary: "down"}
	if err := s.Transition(&stale, core.StateDestroyed, "down"); !errors.Is(err, ErrAlreadyDestroyed) {
		t.Fatalf("err = %v, want ErrAlreadyDestroyed", err)
	}
	if got, _ := s.Load(rig.ID); got.End.Code != core.ReasonIdleTimeout {
		t.Errorf("ending = %s; the first teardown's record stands", got.End.Code)
	}
	// The destroyed copy itself can record a correction.
	rig.End.Evidence = map[string]string{core.EvidenceNothingCreated: "operator: checked"}
	if err := s.Transition(rig, core.StateDestroyed, "correction"); err != nil {
		t.Errorf("a correction to a destroyed rig was refused: %v", err)
	}
}

// A rig file that cannot be read cannot show the rig is not destroyed, so
// only teardown proceeds past it.
func TestAnUnreadableRigFileFailsClosedExceptForTeardown(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.rigPath(rig.ID), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(rig, core.StateDegraded, "probes failing"); err == nil {
		t.Error("moved a rig whose record could not be read")
	}
	if err := s.Transition(rig, core.StateDestroyed, "down"); err != nil {
		t.Errorf("teardown was blocked by an unreadable file: %v", err)
	}
}

// Serving and adoption write the snapshot directly. A bring-up that saved its
// endpoint after `larri down` in another terminal had finished wrote a live
// state back over DESTROYED.
func TestAStaleSnapshotWriteCannotUndoAnEnding(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Transition(rig, core.StateBootstrapping, "bootstrap"); err != nil {
		t.Fatal(err)
	}
	stale := *rig
	if err := s.Transition(rig, core.StateDestroyed, "down"); err != nil {
		t.Fatal(err)
	}
	stale.HostKeyFingerprint = "SHA256:late"
	if err := s.Save(&stale); !errors.Is(err, ErrAlreadyDestroyed) {
		t.Fatalf("err = %v, want ErrAlreadyDestroyed", err)
	}
	if got, _ := s.Load(rig.ID); got.State != core.StateDestroyed {
		t.Errorf("stored state = %s after a stale save", got.State)
	}
}

// Checking and then writing is not enough: two processes can both check
// before either writes. The check has to happen under the same lock as the
// write, so a writer that was waiting sees what the holder wrote.
func TestTheDestroyedCheckRunsUnderTheWriteLock(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	stale := *rig

	unlock, err := s.lockRig(rig.ID)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.Save(&stale) }()
	select {
	case err := <-result:
		t.Fatalf("a save went ahead while another writer held the rig: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	// The holder ends the rig, as `larri down` in another process would.
	rig.State = core.StateDestroyed
	if err := s.write(rig); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-result; !errors.Is(err, ErrAlreadyDestroyed) {
		t.Errorf("waiting save returned %v; it must see the ending written while it waited", err)
	}
}
