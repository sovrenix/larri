// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
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
