// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package ollama

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
)

type recSession struct {
	mu   sync.Mutex
	cmds []string
	out  string
}

func (s *recSession) Run(_ context.Context, cmd string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cmds = append(s.cmds, cmd)
	return []byte(s.out), nil
}
func (s *recSession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (s *recSession) Close() error                                          { return nil }
func (s *recSession) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.cmds, "\n")
}

// FR-SEC-08, and for this engine it is the *whole* of the protection: Ollama
// has no authentication, so a daemon on a routable interface is an open
// inference server. Its container default is 0.0.0.0, which is why the bind is
// set explicitly rather than left alone.
func TestDaemonBindsLoopbackOnly(t *testing.T) {
	cmd := serveCmd()
	if !strings.Contains(cmd, "OLLAMA_HOST=127.0.0.1:8000") {
		t.Errorf("daemon not pinned to loopback:\n%s", cmd)
	}
	if strings.Contains(cmd, "0.0.0.0") {
		t.Errorf("daemon exposed on every interface:\n%s", cmd)
	}
}

// The missing guarantee has to be stated. An operator choosing between engines
// is choosing between security models, and a caveat nobody is shown is not a
// caveat.
func TestSecurityNotesDeclareTheMissingCredential(t *testing.T) {
	notes := New().SecurityNotes()
	if len(notes) == 0 {
		t.Fatal("ollama's lack of authentication went unreported")
	}
	joined := strings.ToLower(strings.Join(notes, " "))
	for _, want := range []string{"no server-side authentication", "loopback", "vllm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes should mention %q, got: %s", want, joined)
		}
	}
}

// Launch must not claim a credential it does not have.
func TestLaunchReportsNoRigCredential(t *testing.T) {
	sess := &recSession{}
	ep, err := New().Launch(context.Background(), sess,
		core.ModelSpec{Ref: "llama3.1:8b", ServedName: "m"}, core.SizingPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Key.Empty() {
		t.Error("ollama reported a rig credential it cannot enforce")
	}
	if ep.Host != runtime.Loopback {
		t.Errorf("host = %q", ep.Host)
	}
}

// FR-RT-04: clients are wired to the served name, so changing the upstream tag
// must not require touching their config.
func TestLaunchAliasesTheTagToTheServedName(t *testing.T) {
	sess := &recSession{}
	if _, err := New().Launch(context.Background(), sess,
		core.ModelSpec{Ref: "llama3.1:8b", ServedName: "coder"}, core.SizingPlan{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sess.all(), "ollama cp 'llama3.1:8b' 'coder'") {
		t.Errorf("served name not aliased:\n%s", sess.all())
	}
}

// The daemon must be up before the pull, because `ollama pull` is a client of
// `ollama serve`. Getting this backwards fails in a way that reads as a
// network problem.
func TestBootstrapStartsTheDaemonBeforePulling(t *testing.T) {
	sess := &recSession{out: "ollama"}
	_ = New().Bootstrap(context.Background(), sess,
		core.ModelSpec{Ref: "llama3.1:8b", ServedName: "m"}, core.SizingPlan{}, nil)
	all := sess.all()
	serve, pull := strings.Index(all, "ollama serve"), strings.Index(all, "ollama pull")
	if serve < 0 || pull < 0 {
		t.Fatalf("expected both a serve and a pull:\n%s", all)
	}
	if serve > pull {
		t.Error("pulled before starting the daemon that serves the pull")
	}
}

func TestProcessPatternsCannotMatchTheirOwnShell(t *testing.T) {
	for _, cmd := range []string{stopServersCmd, aliveCmd, adoptCmd} {
		if strings.Contains(cmd, "ollama serve") {
			t.Errorf("command contains the literal pattern, so it matches its own shell:\n%s", cmd)
		}
	}
}

// With no credential to check, the bind address is all adoption has to go on.
// A daemon on a routable interface is not one LARRI started, and tunnelling to
// it would report a recovered rig while exposing an open server.
func TestAdoptRefusesADaemonNotOnLoopback(t *testing.T) {
	sess := &recSession{out: "OLLAMA_HOST=0.0.0.0:8000\n"}
	_, err := New().Adopt(context.Background(), sess, core.ModelSpec{ServedName: "m"})
	if err == nil {
		t.Fatal("adopted a daemon bound to every interface")
	}
	if !strings.Contains(err.Error(), "not loopback") {
		t.Errorf("error should name the cause, got: %v", err)
	}
}

func TestAdoptAcceptsALoopbackDaemon(t *testing.T) {
	sess := &recSession{out: "OLLAMA_HOST=127.0.0.1:8000\n"}
	ep, err := New().Adopt(context.Background(), sess, core.ModelSpec{ServedName: "m"})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if ep.Port != 8000 || ep.Host != runtime.Loopback {
		t.Errorf("got %s:%d", ep.Host, ep.Port)
	}
}
