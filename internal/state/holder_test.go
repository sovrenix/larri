// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// A reader never sees a rig held by nobody in particular. The record used to
// be the lock file's own contents, truncated and rewritten under the lock, so a
// status taken mid-write read an empty record and named pid 0 as the holder.
func TestAHeldRigAlwaysNamesItsHolder(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			release, err := s.Hold(rig.ID, Holder{PID: os.Getpid(), Detached: true,
				Log: fmt.Sprintf("/s/logs/%d.log", i)})
			if err == nil {
				release()
			}
		}
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h, held, err := s.HolderOf(rig.ID)
		if err != nil {
			t.Fatal(err)
		}
		if held && h.PID == 0 {
			t.Fatal("a held rig was reported with no holder")
		}
	}
	close(stop)
	<-done
}

// A hold whose record cannot be written is not a hold: the process would serve
// a rig that status, resume and logs cannot attribute to it.
func TestAHoldThatCannotBeRecordedFailsAndFreesTheRig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	s := openStore(t)
	rig := newRig(t)
	release, err := s.Hold(rig.ID, Holder{PID: os.Getpid()}) // creates the lock file
	if err != nil {
		t.Fatal(err)
	}
	release()
	rigs := filepath.Join(s.dir, "rigs")
	if err := os.Chmod(rigs, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(rigs, 0o700)
	if _, err := s.Hold(rig.ID, Holder{PID: os.Getpid()}); err == nil {
		t.Fatal("held a rig whose record could not be written")
	}
	os.Chmod(rigs, 0o700)
	again, err := s.Hold(rig.ID, Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("the failed hold kept the lock: %v", err)
	}
	again()
}

// A hold that cannot be inspected is reported as unknown, not as absent:
// calling a supervised rig an orphan misleads the decision it informs.
func TestAnUnreadableHoldIsUnknownNotAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	s := openStore(t)
	rig := newRig(t)
	rig.LocalPort = 8123
	if err := s.Transition(rig, core.StateReady, "ready"); err != nil {
		t.Fatal(err)
	}
	release, err := s.Hold(rig.ID, Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lock := s.holdPath(rig.ID)
	if err := os.Chmod(lock, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(lock, 0o600)
	entries, _ := s.Entries()
	sm := s.Describe(rig, entries, time.Now())
	if sm.HolderErr == "" || sm.Held {
		t.Fatalf("summary %+v; want the hold reported unknown", sm)
	}
	if sm.Endpoint == "" {
		t.Error("an unknown hold was reported as no endpoint, which is the claim of an absent one")
	}
}

// A reader asking who holds a rig does not make taking it fail. The probe takes
// the lock shared for an instant, and a hold attempted in that instant was
// refused as though another process held the rig.
func TestAskingWhoHoldsARigDoesNotStopItBeingTaken(t *testing.T) {
	s := openStore(t)
	rig := newRig(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _, _ = s.HolderOf(rig.ID)
			}
		}
	}()
	for i := 0; i < 300; i++ {
		release, err := s.Hold(rig.ID, Holder{PID: os.Getpid()})
		if err != nil {
			close(stop)
			<-done
			t.Fatalf("attempt %d: %v", i, err)
		}
		release()
	}
	close(stop)
	<-done
}
