// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package vllm

import (
	"context"
	"io"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/runtime"
)

// replySession answers whatever the test dictates, so argv parsing can be
// exercised without a host.
type replySession struct {
	out string
	err error
}

func (s *replySession) Run(context.Context, string) ([]byte, error) {
	return []byte(s.out), s.err
}
func (s *replySession) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (s *replySession) Close() error                                          { return nil }

// argv as /proc/<pid>/cmdline yields it once NULs become newlines.
const runningArgv = `python3
-m
vllm.entrypoints.openai.api_server
--model
Qwen/Qwen3-Coder-30B
--host
127.0.0.1
--port
8000
--served-model-name
qwen3-coder
--api-key
rig-token-abc123
--max-model-len
32768
`

// Recovery hinges on reading back the credential Launch issued. If this
// parse is wrong the tunnel comes up and every request through it is a 401,
// which looks like a broken model rather than a broken adopt.
func TestAdoptRecoversEndpointFromArgv(t *testing.T) {
	ep, err := New().Adopt(context.Background(), &replySession{out: runningArgv}, spec())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got := ep.Key.Reveal(); got != "rig-token-abc123" {
		t.Errorf("key = %q, want rig-token-abc123", got)
	}
	if ep.Port != 8000 {
		t.Errorf("port = %d, want 8000", ep.Port)
	}
	if ep.Model != "qwen3-coder" {
		t.Errorf("model = %q, want qwen3-coder", ep.Model)
	}
	if ep.Host != runtime.Loopback {
		t.Errorf("host = %q, want loopback", ep.Host)
	}
}

// --flag=value is as valid as --flag value, and a runtime that only
// understood one form would fail against an image that spelled it the other.
func TestAdoptAcceptsEqualsForm(t *testing.T) {
	argv := "vllm\nserve\n--port=9001\n--api-key=k\n--served-model-name=m\n"
	ep, err := New().Adopt(context.Background(), &replySession{out: argv}, spec())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if ep.Port != 9001 || ep.Key.Reveal() != "k" || ep.Model != "m" {
		t.Errorf("parsed %d/%q/%q", ep.Port, ep.Key.Reveal(), ep.Model)
	}
}

// A server with no --api-key is not one LARRI started. Adopting it would put
// an unauthenticated endpoint behind the tunnel and report the rig recovered,
// which is worse than failing: the operator would believe the credential
// boundary held.
func TestAdoptRefusesServerWithoutKey(t *testing.T) {
	argv := "vllm\nserve\n--port\n8000\n"
	_, err := New().Adopt(context.Background(), &replySession{out: argv}, spec())
	if err == nil {
		t.Fatal("adopted a server with no api key")
	}
	if !strings.Contains(err.Error(), "not started by larri") {
		t.Errorf("error should name the cause, got: %v", err)
	}
}

func TestAdoptReportsNothingRunning(t *testing.T) {
	_, err := New().Adopt(context.Background(), &replySession{out: "NOTRUNNING\n"}, spec())
	if err == nil {
		t.Fatal("adopted a host with no server")
	}
	if !strings.Contains(err.Error(), "no server running") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The probe must not match the shell that issues it — the same self-matching
// bug that once made a dead process look alive.
func TestAdoptProbeCannotMatchItself(t *testing.T) {
	for _, pat := range []string{"vllm serve", "vllm.entrypoints.openai"} {
		if strings.Contains(adoptCmd, pat) {
			t.Errorf("adoptCmd contains the literal %q, so it matches its own shell", pat)
		}
	}
}
