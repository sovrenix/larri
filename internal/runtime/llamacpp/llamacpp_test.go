// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
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

func spec() core.ModelSpec {
	return core.ModelSpec{Ref: "org/model-GGUF", ServedName: "m", Quantization: "q4_K_M"}
}

func newLaunched() *Runtime {
	r := New()
	r.launcher = "llama-server"
	r.SetGGUF("model.Q4_K_M.gguf")
	return r
}

// FR-SEC-08. The bind address is computed, not configurable, and a runtime
// that published on every interface would be an unauthenticated inference
// server anyone could find.
func TestLaunchBindsLoopbackOnly(t *testing.T) {
	r := newLaunched()
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort, Model: "m",
		Key: secret.New("rig-token")}
	cmd, err := r.launchCommand(spec(), core.SizingPlan{ContextLen: 4096}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "--host '127.0.0.1'") {
		t.Errorf("not bound to loopback:\n%s", cmd)
	}
	if strings.Contains(cmd, "0.0.0.0") {
		t.Errorf("published on every interface:\n%s", cmd)
	}
}

// The offload flag is the whole reason this engine exists in the design: it is
// what lets a model that does not fit in VRAM run anyway.
func TestOffloadLayersArePassedThrough(t *testing.T) {
	r := newLaunched()
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")}
	cmd, err := r.launchCommand(spec(), core.SizingPlan{OffloadLayers: 24}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "-ngl '24'") {
		t.Errorf("offload layers not passed:\n%s", cmd)
	}
}

// The weight-download credential must not reach argv: /proc is readable by a
// host operator who is not you, and this is one of the few secrets here that
// is not already theirs.
func TestHuggingFaceTokenStaysOutOfArgv(t *testing.T) {
	r := newLaunched()
	r.SetHuggingFaceToken(secret.New("hf-secret-value"))
	cmd := r.downloadCmd(spec(), "model.Q4_K_M.gguf")
	if strings.Contains(cmd, "-H \"Authorization: Bearer hf-secret-value\"") {
		t.Errorf("token interpolated into argv:\n%s", cmd)
	}
	if !strings.Contains(cmd, "export HF_TOKEN=") {
		t.Errorf("token should travel through the environment:\n%s", cmd)
	}
}

// The bug that cost a live run under vLLM: a pattern that matches the shell
// issuing it kills its own command.
func TestProcessPatternsCannotMatchTheirOwnShell(t *testing.T) {
	for _, cmd := range []string{stopServersCmd, aliveCmd, adoptCmd} {
		for _, pat := range []string{"llama-server", "/app/server"} {
			if strings.Contains(cmd, pat) {
				t.Errorf("command contains the literal %q, so it matches its own shell:\n%s", pat, cmd)
			}
		}
	}
}

func TestAdoptRecoversEndpointFromArgv(t *testing.T) {
	argv := "llama-server\n--host\n127.0.0.1\n--port\n8000\n--alias\nm\n--api-key\nrig-abc\n"
	ep, err := New().Adopt(context.Background(), &recSession{out: argv}, spec())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if ep.Key.Reveal() != "rig-abc" || ep.Port != 8000 || ep.Model != "m" {
		t.Errorf("parsed %q/%d/%q", ep.Key.Reveal(), ep.Port, ep.Model)
	}
}

func TestAdoptRefusesAServerWithoutOurKey(t *testing.T) {
	argv := "llama-server\n--port\n8000\n"
	if _, err := New().Adopt(context.Background(), &recSession{out: argv}, spec()); err == nil {
		t.Fatal("adopted a server larri did not start")
	}
}

// A repository holds one file per quantisation, and downloading the wrong one
// costs the whole transfer before anything reveals the mistake.
func TestPickQuantChoosesTheRequestedQuantisation(t *testing.T) {
	files := []string{
		"model.Q2_K.gguf", "model.Q4_K_M.gguf", "model.Q8_0.gguf",
	}
	got, err := pickQuant("org/repo", files, "q4_K_M")
	if err != nil {
		t.Fatal(err)
	}
	if got != "model.Q4_K_M.gguf" {
		t.Errorf("chose %q", got)
	}
}

// llama.cpp loads the remaining shards itself, so only the first is ever the
// answer. Offering shard 2 produces a load failure that looks like corruption.
func TestPickQuantTakesTheFirstShardOfASplitModel(t *testing.T) {
	files := []string{
		"model.Q4_K_M-00002-of-00003.gguf",
		"model.Q4_K_M-00001-of-00003.gguf",
		"model.Q4_K_M-00003-of-00003.gguf",
	}
	got, err := pickQuant("org/repo", files, "q4_k_m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "00001-of-") {
		t.Errorf("chose %q, which is not the first shard", got)
	}
}

// A miss must list what the repo does carry — "not found" without the
// alternatives leaves the operator to go and look it up.
func TestMissingQuantisationNamesTheAlternatives(t *testing.T) {
	_, err := pickQuant("org/repo", []string{"model.Q2_K.gguf", "model.Q8_0.gguf"}, "q4_k_m")
	if err == nil {
		t.Fatal("accepted a quantisation the repo does not have")
	}
	if !strings.Contains(err.Error(), "Q2_K") || !strings.Contains(err.Error(), "Q8_0") {
		t.Errorf("error should list what is available, got: %v", err)
	}
}
