// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/state"
)

func logStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func savedRig(t *testing.T, st *state.Store, at time.Time) string {
	t.Helper()
	id, err := state.NewID(at)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(&core.Rig{ID: id, State: core.StateReady, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	return id
}

func heldOnce(t *testing.T, st *state.Store, id string, h state.Holder) {
	t.Helper()
	release, err := st.Hold(id, h)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func writeLog(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A rig is named as docker names a container: its id, or enough of it to be
// unambiguous, in either case.
func TestARigsLogsAreFoundByIDOrPrefix(t *testing.T) {
	st, dir := logStore(t), t.TempDir()
	at := time.Date(2026, 9, 14, 22, 0, 0, 0, time.UTC)
	a, b := savedRig(t, st, at), savedRig(t, st, at) // same millisecond: a shared prefix
	heldOnce(t, st, a, state.Holder{PID: 1, Detached: true, Log: writeLog(t, dir, "a.log", "a\n")})
	heldOnce(t, st, b, state.Holder{PID: 2, Detached: true, Log: writeLog(t, dir, "b.log", "b\n")})

	l, err := FindRigLogs(st, strings.ToLower(a))
	if err != nil || l.Rig != a {
		t.Fatalf("by id: %+v, %v", l, err)
	}
	if _, err := FindRigLogs(st, a[:10]); err == nil || !strings.Contains(err.Error(), "matches 2 rigs") {
		t.Errorf("an ambiguous prefix: err = %v", err)
	}
	if _, err := FindRigLogs(st, "ZZZZ"); err == nil || !strings.Contains(err.Error(), "no rig") {
		t.Errorf("an unknown rig: err = %v", err)
	}
}

// With no rig named, the newest rig with a log is the one wanted — not the
// newest rig, which a foreground `up` may hold with nothing written.
func TestWithNoRigNamedTheNewestLogIsShown(t *testing.T) {
	st, dir := logStore(t), t.TempDir()
	older := savedRig(t, st, time.Date(2026, 9, 14, 21, 0, 0, 0, time.UTC))
	newer := savedRig(t, st, time.Date(2026, 9, 14, 22, 0, 0, 0, time.UTC))
	heldOnce(t, st, older, state.Holder{PID: 1, Detached: true, Log: writeLog(t, dir, "old.log", "x\n")})
	heldOnce(t, st, newer, state.Holder{PID: 2}) // foreground

	l, err := FindRigLogs(st, "")
	if err != nil || l.Rig != older {
		t.Fatalf("got %+v, %v; want the detached rig's log", l, err)
	}
	if _, err := FindRigLogs(st, newer); err == nil || !strings.Contains(err.Error(), "foreground") {
		t.Errorf("a foreground rig: err = %v; want where its output went", err)
	}
	never := savedRig(t, st, time.Date(2026, 9, 14, 23, 0, 0, 0, time.UTC))
	if _, err := FindRigLogs(st, never); err == nil || !strings.Contains(err.Error(), "no detached larri process") {
		t.Errorf("a rig never held: err = %v", err)
	}
}

// A resumed rig's logs read as one account, oldest first, and --tail counts
// from the end of all of it.
func TestLogsReadOldestFirstAndTailFromTheEnd(t *testing.T) {
	st, dir := logStore(t), t.TempDir()
	id := savedRig(t, st, time.Now())
	heldOnce(t, st, id, state.Holder{PID: 1, Detached: true, Log: writeLog(t, dir, "up.log", "one\ntwo\n")})
	latest := writeLog(t, dir, "resume.log", "three\nfour\n")
	heldOnce(t, st, id, state.Holder{PID: 2, Detached: true, Log: latest})

	l, err := FindRigLogs(st, id)
	if err != nil {
		t.Fatal(err)
	}
	all, off, err := l.Read(-1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(all, "two") > strings.Index(all, "three") || !strings.Contains(all, "── "+latest) {
		t.Errorf("not oldest first, or not labelled:\n%s", all)
	}
	if off != int64(len("three\nfour\n")) {
		t.Errorf("offset %d, want the latest log's length", off)
	}
	if tail, _, _ := l.Read(2); tail != "three\nfour\n" {
		t.Errorf("tail 2 = %q", tail)
	}
	if none, off0, _ := l.Read(0); none != "" || off0 != off {
		t.Errorf("tail 0 = %q at %d; want nothing, and following from the end", none, off0)
	}
}
