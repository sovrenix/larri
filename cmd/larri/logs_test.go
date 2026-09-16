// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/state"
)

// Flags go before or after the rig, as `docker logs` takes them.
func TestLogsFlagsGoEitherSideOfTheRig(t *testing.T) {
	for _, args := range [][]string{{"01K2", "-f", "-n", "5"}, {"-f", "01K2", "--tail=5"}, {"-n", "5", "-f", "01K2"}} {
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		follow := fs.Bool("f", false, "")
		tail := fs.String("n", "all", "")
		fs.StringVar(tail, "tail", "all", "")
		refs := parseInterleaved(fs, args)
		if len(refs) != 1 || refs[0] != "01K2" || !*follow || *tail != "5" {
			t.Errorf("%v: refs %v follow %v tail %q", args, refs, *follow, *tail)
		}
	}
}

func TestLogsTailMustBeANumberOrAll(t *testing.T) {
	t.Setenv("LARRI_STATE_DIR", t.TempDir())
	if err := cmdLogs(context.Background(), []string{"-n", "ten"}); err == nil || !strings.Contains(err.Error(), "--tail") {
		t.Errorf("err = %v", err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("never printed %q:\n%s", want, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// Following carries on past the release of the hold — a holder lets go before
// it prints how the rig ended — moves to the log of a process that takes the
// rig over, and stops once nobody holds the rig and its last holder has gone.
func TestFollowingALogOutlastsTheReleaseAndStopsWithTheHolder(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := state.NewID(time.Now())
	if err := st.Save(&core.Rig{ID: id, State: core.StateReady, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "up.log")
	appendTo(t, first, "printed before following\n")
	release, err := st.Hold(id, state.Holder{PID: os.Getpid(), Detached: true, Log: first})
	if err != nil {
		t.Fatal(err)
	}
	l, err := daemon.FindRigLogs(st, id)
	if err != nil {
		t.Fatal(err)
	}
	_, offset, _ := l.Read(0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- followLogs(ctx, st, l, offset, &out, 5*time.Millisecond) }()

	appendTo(t, first, "weights.download 40%\n")
	waitFor(t, &out, "weights.download 40%")
	release()
	appendTo(t, first, "rig DESTROYED\n") // after the release, from a holder still running
	waitFor(t, &out, "rig DESTROYED")

	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Skipf("no process to stand in for an exited holder: %v", err)
	}
	second := filepath.Join(dir, "resume.log")
	appendTo(t, second, "reconnected\n")
	release2, err := st.Hold(id, state.Holder{PID: gone.Process.Pid, Detached: true, Log: second})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, &out, "holds the rig now: "+second)
	waitFor(t, &out, "reconnected")
	release2()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("follow: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still following a rig nobody holds, after its holder exited")
	}
	if strings.Contains(out.String(), "printed before following") {
		t.Errorf("repeated what was already shown:\n%s", out.String())
	}
}

// A rig a foreground process holds has logs only from earlier holders, and no
// more will be written to them: following says where the output goes now,
// rather than polling a file nobody writes — at the start, or when a foreground
// process takes the rig over.
func TestFollowingARigAForegroundProcessHoldsSaysWhereItsOutputGoes(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _ := state.NewID(time.Now())
	if err := st.Save(&core.Rig{ID: id, State: core.StateReady, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	up := filepath.Join(dir, "up.log")
	appendTo(t, up, "detached bring-up\n")

	// A detached holder, followed, and then a foreground one taking over.
	release, err := st.Hold(id, state.Holder{PID: os.Getpid(), Detached: true, Log: up})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := daemon.FindRigLogs(st, id)
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- followLogs(context.Background(), st, l, 0, &out, 5*time.Millisecond) }()
	waitFor(t, &out, "detached bring-up")
	release()
	fg, err := st.Hold(id, state.Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer fg()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kept following after a foreground process took the rig")
	}
	if !strings.Contains(out.String(), "in the foreground") {
		t.Errorf("did not say where the output went:\n%s", out.String())
	}

	// Asked to follow while the foreground process holds it: said at once.
	l, err = daemon.FindRigLogs(st, id)
	if err != nil {
		t.Fatal(err)
	}
	var now syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := followLogs(ctx, st, l, 0, &now, 5*time.Millisecond); err != nil || !strings.Contains(now.String(), "in the foreground") {
		t.Errorf("err %v, output %q", err, now.String())
	}
	if ctx.Err() != nil {
		t.Error("followed until the deadline instead of saying at once")
	}
}
