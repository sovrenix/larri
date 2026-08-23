// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/state"
)

// Two concurrent bring-ups mean two billing instances, and an agent is
// likelier than a person to ask twice — it cannot see the terminal it did not
// print to.
func TestSessionRefusesASecondBringUp(t *testing.T) {
	s := &Session{}
	if !s.Begin("rig-1", func() {}) {
		t.Fatal("the first bring-up was refused")
	}
	if s.Begin("rig-2", func() {}) {
		t.Fatal("a second bring-up was allowed alongside the first")
	}
}

func TestSessionReportsProgress(t *testing.T) {
	s := &Session{}
	s.Begin("rig-1", func() {})
	s.Note("boot", "pulling image")

	snap := s.Snapshot()
	if snap.Phase != "boot" || snap.Message != "pulling image" {
		t.Errorf("snapshot = %+v", snap)
	}
	if !snap.Running {
		t.Error("a bring-up in flight did not report as running")
	}
	if snap.Elapsed <= 0 {
		t.Error("no elapsed time; a poll cannot tell a slow boot from a stuck one")
	}
}

// Stop releases the tunnel. It must not destroy: teardown is a provider call
// with a termination record, and conflating "stop holding this" with "stop
// paying for this" is how a dropped session becomes an instance nobody tracks.
func TestSessionStopCancelsButDoesNotDestroy(t *testing.T) {
	var cancelled bool
	s := &Session{}
	s.Begin("rig-1", func() { cancelled = true })
	s.Stop()

	if !cancelled {
		t.Error("the bring-up was not cancelled")
	}
	if s.Snapshot().Running {
		t.Error("still reporting as running after a stop")
	}
	// Releasing must free the slot, or a stopped session would block the next
	// bring-up forever.
	if !s.Begin("rig-2", func() {}) {
		t.Error("a stopped session did not release the slot")
	}
}

func TestSessionCarriesFailureToTheCaller(t *testing.T) {
	s := &Session{}
	s.Begin("rig-1", func() {})
	s.Fail(errors.New("no offer satisfies the criteria"))

	snap := s.Snapshot()
	if snap.Err == nil || !strings.Contains(snap.Err.Error(), "no offer") {
		t.Errorf("failure not reported: %+v", snap)
	}
	if snap.Running {
		t.Error("a failed bring-up still reports as running")
	}
}

// The endpoint and key are the whole point of holding a rig: without them the
// agent has paid for a model it cannot reach.
func TestStatusReportsTheServingEndpoint(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := &Session{}
	s.Begin("rig-1", func() {})
	s.Note("ready", "serving")

	d := Deps{Store: st, Session: s}
	out, err := d.status(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	held, ok := m["serving"].(map[string]any)
	if !ok {
		t.Fatal("status said nothing about the rig being brought up")
	}
	if held["phase"] != "ready" {
		t.Errorf("phase = %v", held["phase"])
	}
}

// A bring-up can be in flight before any instance exists. A `down` that
// reported "nothing to tear down" while provisioning continued would be the
// one command that made the leak worse.
func TestDownCancelsABringUpWithNoInstanceYet(t *testing.T) {
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var cancelled bool
	s := &Session{}
	s.Begin("", func() { cancelled = true })

	d := Deps{Store: st, Session: s}
	out, err := d.down(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled {
		t.Fatal("down left an in-flight bring-up running")
	}
	m := out.(map[string]any)
	if m["stopped"] != true {
		t.Errorf("down did not report stopping the bring-up: %v", m)
	}
	if !strings.Contains(m["note"].(string), "larri_orphans") {
		t.Error("did not point at how to confirm nothing was created")
	}
}

// larri_logs needs the live session; without one it must say so rather than
// return an empty log that reads like a silent runtime.
func TestLogsWithoutASessionExplainsItself(t *testing.T) {
	d := Deps{Session: &Session{}}
	if _, err := d.logs(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("returned logs with no rig")
	}
}

var _ = daemon.Live{}
