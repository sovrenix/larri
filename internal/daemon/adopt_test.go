// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/state"
)

// upRig provisions a rig so the adopt paths have something to reconcile
// against, exactly as a restart would find on disk.
func upRig(t *testing.T) (*Orchestrator, *pfake.Provider, *core.Rig) {
	t.Helper()
	o, p, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	o.dropUpHold() // the process that rented it has gone, as after a restart
	return o, p, rig
}

// The costly mistake this guards: a rig whose instance the provider has
// already reclaimed must be recorded as gone rather than reconnected to.
// Leaving it in a live state keeps the cost accountant billing against a
// machine that stopped existing.
func TestAdoptRecordsAnInstanceThatVanished(t *testing.T) {
	o, p, rig := upRig(t)
	p.Vanish(rig.Instance.InstanceID)

	if _, err := o.Adopt(context.Background(), rig.ID); err == nil {
		t.Fatal("adopted a vanished instance")
	}
	got, err := o.Store.Load(rig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", got.State)
	}
}

// STOPPED still bills for storage (§12.4). Adopt must surface that as a
// decision rather than silently resume — a rig the operator meant to be rid
// of should not come back because a process restarted.
func TestAdoptRefusesToResumeAStoppedRig(t *testing.T) {
	o, p, rig := upRig(t)
	p.Stop(rig.Instance.InstanceID)

	_, err := o.Adopt(context.Background(), rig.ID)
	if err == nil {
		t.Fatal("adopted a stopped instance")
	}
	if !strings.Contains(err.Error(), "billing storage") {
		t.Errorf("error should name the ongoing cost, got: %v", err)
	}
	got, _ := o.Store.Load(rig.ID)
	if got.State != core.StateStopped {
		t.Errorf("state = %s, want STOPPED", got.State)
	}
}

// A provider that cannot install a key on a running instance leaves the rig
// destroyable but not reconnectable. Saying so plainly beats a timeout that
// looks like a network fault.
func TestAdoptReportsWhenTheProviderCannotAttachAKey(t *testing.T) {
	o, _, rig := upRig(t)

	_, err := o.Adopt(context.Background(), rig.ID)
	if err == nil {
		t.Fatal("adopted through a provider with no key-attach capability")
	}
	if !strings.Contains(err.Error(), "teardown only") {
		t.Errorf("error should say teardown still works, got: %v", err)
	}
}

func TestAdoptRefusesADestroyedRig(t *testing.T) {
	o, _, rig := upRig(t)
	if err := o.Store.Transition(rig, core.StateDestroyed, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Adopt(context.Background(), rig.ID); err == nil {
		t.Fatal("adopted a destroyed rig")
	}
}

// The ending that adoption writes says why, like every other: the journal
// entry had no termination, so status and replay could not explain it.
func TestAdoptOfAVanishedInstanceRecordsWhy(t *testing.T) {
	o, p, rig := upRig(t)
	p.Vanish(rig.Instance.InstanceID)
	_, _ = o.Adopt(context.Background(), rig.ID)

	entries, _ := o.Store.Entries()
	last := entries[len(entries)-1]
	if last.To != core.StateDestroyed || last.Termination == nil ||
		last.Termination.Code != core.ReasonInstanceGone || last.Termination.At.IsZero() {
		t.Errorf("ending entry = %+v; want a dated instance-gone termination", last.Termination)
	}
}

// A snapshot from before rates were journalled in dollars an hour holds Vast's
// per-month storage figure. The STOPPED entry adoption writes must carry the
// provider's current figure, or every stopped hour bills at the monthly one.
func TestAdoptJournalsTheProvidersRateNotTheSnapshots(t *testing.T) {
	o, p, rig := upRig(t)
	p.Stop(rig.Instance.InstanceID)
	stale, _ := o.Store.Load(rig.ID)
	stale.Instance.StorageHr = 0.20 // $/GB/month, as old snapshots hold it
	if err := o.Store.Save(stale); err != nil {
		t.Fatal(err)
	}
	fresh, _ := p.Get(context.Background(), rig.Instance.InstanceID)

	_, _ = o.Adopt(context.Background(), rig.ID)
	entries, _ := o.Store.Entries()
	last := entries[len(entries)-1]
	if last.To != core.StateStopped || last.StorageHr != fresh.StorageHr {
		t.Errorf("STOPPED entry storage = %v, want the provider's %v", last.StorageHr, fresh.StorageHr)
	}
}

// A rig a detached `larri up` is still serving is not reconnected to a second
// time: two holders would run two tunnels and two supervisors.
func TestAdoptRefusesARigAnotherProcessHolds(t *testing.T) {
	o, _, rig := upRig(t)
	release, err := o.Store.Hold(rig.ID, state.Holder{PID: 4242, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = o.Adopt(context.Background(), rig.ID)
	if err == nil || !strings.Contains(err.Error(), "already served by larri pid 4242") {
		t.Fatalf("err = %v; want the holder named", err)
	}
}

// An adoption that fails lets go of the rig, so the next attempt is not
// refused by the attempt that failed.
func TestAFailedAdoptReleasesTheRig(t *testing.T) {
	o, p, rig := upRig(t)
	p.Stop(rig.Instance.InstanceID) // adopt refuses a stopped instance
	if _, err := o.Adopt(context.Background(), rig.ID); err == nil {
		t.Fatal("adopted a stopped instance")
	}
	if _, held, _ := o.Store.HolderOf(rig.ID); held {
		t.Error("a failed adopt kept holding the rig")
	}
}

// A rig is held from the moment it has an id, not from the moment it serves:
// most of a bring-up is a download, and `larri status` has to be able to say
// who is renting the rig while it happens. The Serve that follows takes the
// same hold rather than being refused by it.
func TestARigIsHeldFromTheMomentItIsRented(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, held, _ := o.Store.HolderOf(rig.ID); !held {
		t.Fatal("a rig being brought up has no holder")
	}
	if _, err := o.Store.Hold(rig.ID, state.Holder{PID: 1}); !errors.Is(err, state.ErrHeld) {
		t.Fatalf("another process took a rig being brought up: err = %v", err)
	}
	release, err := o.takeHold(rig, "test")
	if err != nil {
		t.Fatalf("Serve was refused the hold Up took: %v", err)
	}
	if o.upHold != nil {
		t.Error("the hold was handed over and still kept for another Serve")
	}
	release()
	if _, held, _ := o.Store.HolderOf(rig.ID); held {
		t.Error("released, and still held")
	}
}
