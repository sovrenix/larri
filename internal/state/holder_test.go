// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"errors"
	"os"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// A rig has at most one holder. A second — `larri resume` while a detached
// `larri up` still serves — would run a second tunnel and supervisor, each
// reading the other's traffic as someone else's.
func TestARigHasOneHolderAtATime(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	release, err := s.Hold(rig.ID, Holder{PID: os.Getpid(), Started: time.Now(), Log: "/tmp/x.log", Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	h, held, err := s.HolderOf(rig.ID)
	if err != nil || !held || h.PID != os.Getpid() || !h.Detached {
		t.Fatalf("holder = %+v held=%v err=%v", h, held, err)
	}
	if _, err := s.Hold(rig.ID, Holder{PID: 1}); !errors.Is(err, ErrHeld) {
		t.Errorf("second hold err = %v; a rig took a second holder", err)
	}
	release()
	h, held, err = s.HolderOf(rig.ID)
	if err != nil || held {
		t.Errorf("released rig still reads as held: %+v %v", h, err)
	}
	if h.Log != "/tmp/x.log" {
		t.Errorf("the last holder's record was lost: %+v", h)
	}
	again, err := s.Hold(rig.ID, Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("a released rig could not be held again: %v", err)
	}
	again()
}

func TestARigNeverHeldHasNoHolder(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	if _, held, err := s.HolderOf(rig.ID); held || err != nil {
		t.Errorf("held=%v err=%v for a rig nobody held", held, err)
	}
}

// A billing rig nobody holds has no endpoint and no supervisor, and every
// surface is told so rather than shown an address that answers nothing.
func TestARigNobodyHoldsHasNoEndpoint(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	rig.LocalPort = 8123
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.Entries()
	if sm := s.Describe(rig, entries, time.Now()); sm.Held || sm.Endpoint != "" {
		t.Errorf("unheld rig: held=%v endpoint=%q", sm.Held, sm.Endpoint)
	}
	release, err := s.Hold(rig.ID, Holder{PID: os.Getpid(), Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if sm := s.Describe(rig, entries, time.Now()); !sm.Held || sm.Endpoint == "" || !sm.Holder.Detached {
		t.Errorf("held rig: %+v", sm)
	}
}

// A rig resumed after its holder died keeps the dead holder's log: it is the
// account of why the rig needed resuming.
func TestEveryHoldersLogIsKept(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	for _, log := range []string{"/s/logs/up.log", "", "/s/logs/resume.log"} {
		release, err := s.Hold(rig.ID, Holder{PID: os.Getpid(), Log: log, Detached: log != ""})
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	h, _, err := s.HolderOf(rig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Logs(); len(got) != 2 || got[0] != "/s/logs/up.log" || got[1] != "/s/logs/resume.log" {
		t.Errorf("logs = %v; want both detached holders' logs, oldest first", got)
	}
}
