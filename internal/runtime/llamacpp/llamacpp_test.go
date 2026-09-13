// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package llamacpp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
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

// A live run died here: /app/llama-server exists and is executable, but a
// non-interactive ssh session inherits none of the image's ENV, so it exits
// immediately with "error while loading shared libraries:
// libllama-server-impl.so". The binary and its shared objects ship in the same
// directory.
func TestLaunchSetsTheLibraryPathForAnImageLocalBinary(t *testing.T) {
	r := New()
	r.launcher = "/app/llama-server"
	r.SetGGUF("model.gguf")
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")}
	cmd, err := r.launchCommand(spec(), core.SizingPlan{}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "LD_LIBRARY_PATH='/app'") {
		t.Errorf("the image's own library directory is not on the search path:\n%s", cmd)
	}
}

// A launcher found on PATH needs no such help, and adding an empty or "."
// entry to the search path would be its own kind of wrong.
func TestLaunchLeavesThePathAloneForAPathBinary(t *testing.T) {
	r := New()
	r.launcher = "llama-server"
	r.SetGGUF("model.gguf")
	ep := runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort, Key: secret.New("k")}
	cmd, err := r.launchCommand(spec(), core.SizingPlan{}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "LD_LIBRARY_PATH") {
		t.Errorf("needlessly rewrote the library path:\n%s", cmd)
	}
}

// Every part has to arrive. A model large enough to need more than one card
// is published in parts, so for this engine that is the case rather than an
// edge case — and fetching only the first left the engine failing on a
// missing tensor, which reads exactly like a corrupt download.
func TestBootstrapFetchesEveryShard(t *testing.T) {
	r := New()
	r.SetGGUF("UD-Q4_K_XL/model-00001-of-00003.gguf")
	sess := &recSession{out: "llama-server"}
	if err := r.Bootstrap(context.Background(), sess, spec(), core.SizingPlan{}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		part := fmt.Sprintf("model-%05d-of-00003.gguf", i)
		var seen bool
		for _, c := range sess.cmds {
			if strings.Contains(c, part) {
				seen = true
			}
		}
		if !seen {
			t.Errorf("part %d (%s) was never fetched", i, part)
		}
	}
}

// The URL keeps the repository's path and the destination does not: the
// download created the model directory but never the quantisation directory
// inside it, so curl failed to open its destination before a byte moved.
func TestDownloadWritesBesideTheModelDirNotUnderIt(t *testing.T) {
	r := New()
	cmd := r.downloadCmd(spec(), "UD-Q4_K_XL/model-00001-of-00003.gguf")
	if !strings.Contains(cmd, "resolve/main/UD-Q4_K_XL/model-00001-of-00003.gguf") {
		t.Errorf("the URL lost the repository path: %s", cmd)
	}
	if strings.Contains(cmd, ModelDir+"/UD-Q4_K_XL/") {
		t.Errorf("the destination is a directory nothing creates: %s", cmd)
	}
	if !strings.Contains(cmd, ModelDir+"/model-00001-of-00003.gguf") {
		t.Errorf("the destination is not flattened into the model dir: %s", cmd)
	}
}

// And the launch has to be pointed at the file that was actually written.
func TestLaunchPointsAtTheFlattenedFile(t *testing.T) {
	r := New()
	r.launcher = "llama-server"
	r.SetGGUF("UD-Q4_K_XL/model-00001-of-00003.gguf")
	cmd, err := r.launchCommand(spec(), core.SizingPlan{ContextLen: 4096},
		runtime.Endpoint{Host: runtime.Loopback, Port: RemotePort, Model: "m",
			Key: secret.New("k")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, ModelDir+"/model-00001-of-00003.gguf") {
		t.Errorf("launch points somewhere nothing was downloaded: %s", cmd)
	}
}

// scriptedSession answers each command by what it contains, so a test can
// give the free-space probe one answer and the runtime probe another.
type scriptedSession struct {
	recSession
	answers map[string]string
}

func (s *scriptedSession) Run(ctx context.Context, cmd string) ([]byte, error) {
	s.recSession.Run(ctx, cmd)
	for marker, out := range s.answers {
		if strings.Contains(cmd, marker) {
			return []byte(out), nil
		}
	}
	return []byte(s.out), nil
}

// A disk that cannot hold the weights is found before the download, not
// twenty gigabytes into it. On RunPod this is what catches a volume link the
// start script was allowed not to make.
func TestBootstrapRefusesADiskThatCannotHoldTheWeights(t *testing.T) {
	r := New()
	r.SetWeights(Weights{File: "m-00001-of-00002.gguf", Bytes: 100 << 30})
	sess := &scriptedSession{
		recSession: recSession{out: "llama-server"},
		// 20 GiB free, nothing there yet: the container disk, not the volume.
		answers: map[string]string{"df -Pk": "20971520\n0\n"},
	}
	err := r.Bootstrap(context.Background(), sess, spec(), core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("a 20 GB disk was accepted for 100 GB of weights")
	}
	if !errs.Is(err, errs.ClassHostFailure) {
		t.Errorf("class = %s; the next host may have the space", errs.ClassOf(err))
	}
	for _, c := range sess.cmds {
		if strings.Contains(c, "curl") {
			t.Fatal("a download started on a disk that could not hold it")
		}
	}
}

// Space already used by the same weights counts as space: a retried
// bootstrap pays only for what is missing.
func TestBootstrapCountsWhatIsAlreadyDownloaded(t *testing.T) {
	r := New()
	r.SetWeights(Weights{File: "m.gguf", Bytes: 30 << 30})
	sess := &scriptedSession{
		recSession: recSession{out: "llama-server"},
		// 10 GiB free, 25 GiB already down.
		answers: map[string]string{"df -Pk": "10485760\n26214400\n"},
	}
	if err := r.Bootstrap(context.Background(), sess, spec(), core.SizingPlan{}, nil); err != nil {
		t.Fatalf("refused a download that fits once what is already there counts: %v", err)
	}
}

// A probe that cannot run proves nothing, and an unmeasured size gives it
// nothing to compare against. Both pass (§4a).
func TestFreeSpaceCheckPassesWhenItCannotMeasure(t *testing.T) {
	for name, c := range map[string]struct {
		bytes uint64
		out   string
	}{
		"no df output":     {100 << 30, ""},
		"garbage":          {100 << 30, "Filesystem\n"},
		"no measured size": {0, "1\n0\n"},
	} {
		r := New()
		r.SetWeights(Weights{File: "m.gguf", Bytes: c.bytes})
		sess := &scriptedSession{recSession: recSession{out: "llama-server"},
			answers: map[string]string{"df -Pk": c.out}}
		if err := r.checkFreeSpace(context.Background(), sess); err != nil {
			t.Errorf("%s: refused on no evidence: %v", name, err)
		}
	}
}
