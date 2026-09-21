// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/claw/comfyui/workflow"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

// hostSim is a rented host that behaves: it has python and torch, no ComfyUI
// until it is installed, and a models directory that fills up as the fetch
// script runs.
//
// It answers the same commands the adapter actually issues, so a change to
// those commands that breaks the host contract breaks this test rather than
// the next rental. That is the gap the project keeps falling into — code that
// passes its unit tests and fails on real hardware — and a host simulator is
// the closest a free test can get to closing it.
type hostSim struct {
	mu  sync.Mutex
	ran []string

	hasComfy   bool
	installed  bool
	fetchSteps int // how many polls before the fetch completes
	polls      int //
	bytes      uint64
	total      uint64
	launched   bool
	outputs    map[string][]byte
	fetchFails string
	smokeFails string
}

const newline = "\n"

func newHost(total uint64, steps int) *hostSim {
	return &hostSim{fetchSteps: steps, total: total, outputs: map[string][]byte{}}
}

func (h *hostSim) Run(_ context.Context, cmd string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, cmd)

	switch {
	case strings.Contains(cmd, "for p in"):
		// The probe searches known layouts, not just PATH, because a
		// non-interactive SSH session never sees the conda PATH a
		// pytorch/pytorch image sets in its Dockerfile. The simulator
		// answers as that image does.
		return []byte("FOUND /opt/conda/bin/python\n"), nil

	case strings.Contains(cmd, "main.py") && strings.Contains(cmd, "test -f"):
		if h.hasComfy {
			return []byte("HAVE\n"), nil
		}
		return []byte("MISSING\n"), nil

	case strings.Contains(cmd, "git clone"):
		h.installed = true
		h.hasComfy = true
		return []byte("INSTALLED\n"), nil

	case strings.Contains(cmd, "import nodes"):
		// The import check that runs between the install and the download,
		// so a torch too old for this ComfyUI is found in seconds rather
		// than after six gigabytes of weights.
		if h.smokeFails != "" {
			return []byte(h.smokeFails + "\n"), nil
		}
		return []byte("SMOKE_OK\n"), nil

	case strings.Contains(cmd, "cat > ") && strings.Contains(cmd, "fetch"):
		return nil, nil

	case strings.Contains(cmd, launchScriptPath) && strings.Contains(cmd, "setsid"):
		// The launch and the fetch now share a shape, so they are told apart
		// by which script is being run rather than by the shape itself.
		h.launched = true
		return []byte("LAUNCHED" + newline), nil

	case strings.Contains(cmd, "nohup sh"):
		return []byte("STARTED\n"), nil

	case strings.Contains(cmd, "test -f") && strings.Contains(cmd, ".done"):
		h.polls++
		// Bytes land progressively, which is what the stall detector reads.
		if h.polls < h.fetchSteps {
			h.bytes = h.total * uint64(h.polls) / uint64(h.fetchSteps)
			return []byte("NO\n"), nil
		}
		h.bytes = h.total
		return []byte("YES\n"), nil

	case strings.Contains(cmd, "tail -n 20"):
		if h.fetchFails != "" {
			return []byte(h.fetchFails + "\n"), nil
		}
		return nil, nil

	case strings.Contains(cmd, "du -sb"):
		return []byte(fmt.Sprint(h.bytes) + "\n"), nil

	case strings.Contains(cmd, "main.py --listen"):
		h.launched = true
		return []byte("LAUNCHED\n"), nil

	case strings.HasPrefix(cmd, "pgrep"):
		if h.launched {
			return []byte("ALIVE\n"), nil
		}
		return []byte("GONE\n"), nil

	case strings.Contains(cmd, "find "):
		var b strings.Builder
		for name, data := range h.outputs {
			fmt.Fprintf(&b, "%d\t1700000000\t%s\x00", len(data), name)
		}
		return []byte(b.String()), nil

	case strings.HasPrefix(cmd, "base64 "):
		for name, data := range h.outputs {
			if strings.Contains(cmd, name) {
				return []byte(encodeBase64(data)), nil
			}
		}
		return nil, nil
	}
	return nil, nil
}

func (h *hostSim) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (h *hostSim) Close() error                                          { return nil }

func (h *hostSim) render(name string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outputs[name] = data
}

var _ runtime.Session = (*hostSim)(nil)

// lifecycleGraph is a complete SDXL text-to-image workflow.
const lifecycleGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple",
        "inputs": {"ckpt_name": "base.safetensors"}},
  "11": {"class_type": "VAELoader", "inputs": {"vae_name": "vae.safetensors"}},
  "5": {"class_type": "EmptyLatentImage",
        "inputs": {"width": 1024, "height": 1024, "batch_size": 1}},
  "3": {"class_type": "KSampler", "inputs": {"steps": 20}},
  "9": {"class_type": "SaveImage", "inputs": {"images": ["8", 0]}}
}`

// One workflow, all the way through: parse it, measure it, install ComfyUI,
// fetch the models onto the host, launch bound to loopback, prove a render
// round-trips, and collect the image locally.
//
// The transport is the only thing faked. That is the deliberate seam: SSH and
// the provider API are exercised by the paid suite in e2e_comfy_test.go, and
// everything between them runs here for nothing, on every commit.
func TestOneWorkflowAllTheWayThrough(t *testing.T) {
	ctx := context.Background()

	// ---- before the money -------------------------------------------------
	g, err := workflow.Parse([]byte(lifecycleGraph))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !g.Format.Executable() {
		t.Fatal("the api serialisation must be submittable")
	}
	m := &workflow.Manifest{Models: map[string]workflow.Source{
		"base.safetensors": {Repo: "r/base", File: "base.safetensors", Bytes: 6_940_000_000},
		"vae.safetensors":  {Repo: "r/vae", File: "vae.safetensors", Bytes: 335_000_000},
	}}
	bundle, err := workflow.Resolve(ctx, g, nil, workflow.ResolveOptions{Manifest: m})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const wantTotal = 6_940_000_000 + 335_000_000
	if bundle.TotalBytes != wantTotal {
		t.Fatalf("bundle = %d bytes, want %d", bundle.TotalBytes, wantTotal)
	}

	urls := map[string]string{
		"checkpoints/base.safetensors": "https://hf.invalid/r/base/resolve/main/base.safetensors",
		"vae/vae.safetensors":          "https://hf.invalid/r/vae/resolve/main/vae.safetensors",
	}
	rt := New(g, bundle, urls)
	rt.PollInterval = time.Millisecond
	rt.SetHuggingFaceToken(secret.New("hf_test_token"))

	host := newHost(wantTotal, 4)

	// ---- bootstrap: install, then fetch onto the host ---------------------
	progress := make(chan runtime.Progress, 64)
	var phases []runtime.Phase
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range progress {
			phases = append(phases, p.Phase)
		}
	}()
	spec := core.ModelSpec{Ref: "sdxl", ServedName: "comfyui"}
	if err := rt.Bootstrap(ctx, host, spec, core.SizingPlan{}, progress); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	close(progress)
	wg.Wait()

	if !host.installed {
		t.Error("comfyui was never installed on a host that did not carry it")
	}
	if !contains(phases, runtime.PhaseWeightsDownload) {
		t.Errorf("no weight-download progress was reported: %v — a multi-GB "+
			"fetch that reports nothing looks like a hang", phases)
	}

	// The credential reached the host exactly once, in a file, and never in
	// the fetch script.
	var tokenCommands int
	for _, c := range host.commands() {
		if strings.Contains(c, "hf_test_token") {
			tokenCommands++
		}
	}
	if tokenCommands != 1 {
		t.Errorf("the token appeared in %d commands, want 1", tokenCommands)
	}

	// ---- launch ----------------------------------------------------------
	ep, err := rt.Launch(ctx, host, spec, core.SizingPlan{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if ep.Host != runtime.Loopback {
		t.Errorf("bound %s, want loopback", ep.Host)
	}
	if alive, _ := rt.Alive(ctx, host); !alive {
		t.Error("the server is not running after launch")
	}

	// ---- readiness: a real render round-trip -----------------------------
	stub := newStub(true, []OutputFile{{Filename: "ComfyUI_00001_.png", Type: "output"}})
	defer stub.Close()
	if err := rt.Ready(ctx, runtime.Endpoint{
		Host: "127.0.0.1", Port: stubPort(t, stub)}, spec); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if stub.submissions() == 0 {
		t.Fatal("readiness did not submit the operator's graph")
	}

	// ---- the session renders --------------------------------------------
	png := []byte("\x89PNG\r\n\x1a\nrendered pixels")
	host.render("ComfyUI_00001_.png", png)
	// ComfyUI ships this zero-byte placeholder in its output directory. A live
	// run collected it beside the one real image and reported "2 saved".
	host.render("_output_images_will_be_put_here", nil)

	// ---- collect before teardown ----------------------------------------
	local := t.TempDir()
	res, err := Sync(ctx, host, local, SyncOptions{})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("collection incomplete: %+v", res.Failed)
	}
	if len(res.Saved) != 1 {
		t.Fatalf("saved %d files, want 1: %v", len(res.Saved), res.Saved)
	}
	if _, err := os.Stat(filepath.Join(local, "_output_images_will_be_put_here")); err == nil {
		t.Error("the empty placeholder was collected and counted as a render")
	}
	got, err := os.ReadFile(filepath.Join(local, "ComfyUI_00001_.png"))
	if err != nil {
		t.Fatalf("the render did not reach local disk: %v", err)
	}
	if string(got) != string(png) {
		t.Error("the saved image does not match what the host rendered")
	}

	// ---- stop ------------------------------------------------------------
	if err := rt.Stop(ctx, host); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A fetch that stops moving must end the wait rather than bill out the cap.
// The clock runs on silence, not on elapsed time (§12.2.1), so a host still
// making progress is never discarded.
func TestAStalledFetchEndsTheWait(t *testing.T) {
	g, _ := workflow.Parse([]byte(lifecycleGraph))
	m := &workflow.Manifest{Models: map[string]workflow.Source{
		"base.safetensors": {Repo: "r/b", File: "b.safetensors", Bytes: 1000},
		"vae.safetensors":  {Repo: "r/v", File: "v.safetensors", Bytes: 1000},
	}}
	bundle, _ := workflow.Resolve(context.Background(), g, nil,
		workflow.ResolveOptions{Manifest: m})

	rt := New(g, bundle, map[string]string{
		"checkpoints/base.safetensors": "https://x.invalid/b", "vae/vae.safetensors": "https://x.invalid/v",
	})
	rt.PollInterval = time.Millisecond
	rt.FetchStall = 20 * time.Millisecond
	rt.FetchCap = 5 * time.Second

	// A host that starts the fetch and then stops writing bytes.
	host := newHost(2000, 1_000_000)
	err := rt.Bootstrap(context.Background(), host,
		core.ModelSpec{}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("a stalled fetch ran to the cap instead of ending on silence")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error does not name the stall: %v", err)
	}
}

// A fetch that reports a failure must not be waited out either: the outcome is
// already decided and every further second is billed for an answer that has
// arrived.
func TestAFailedFetchIsReportedImmediately(t *testing.T) {
	g, _ := workflow.Parse([]byte(lifecycleGraph))
	m := &workflow.Manifest{Models: map[string]workflow.Source{
		"base.safetensors": {Repo: "r/b", File: "b.safetensors", Bytes: 1000},
		"vae.safetensors":  {Repo: "r/v", File: "v.safetensors", Bytes: 1000},
	}}
	bundle, _ := workflow.Resolve(context.Background(), g, nil,
		workflow.ResolveOptions{Manifest: m})
	rt := New(g, bundle, map[string]string{
		"checkpoints/base.safetensors": "https://x.invalid/b", "vae/vae.safetensors": "https://x.invalid/v",
	})
	rt.PollInterval = time.Millisecond

	host := newHost(2000, 1_000_000)
	host.fetchFails = "FAILED base.safetensors"

	err := rt.Bootstrap(context.Background(), host,
		core.ModelSpec{}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("a failed fetch was treated as still in progress")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error does not name the failure: %v", err)
	}
}

// A host that already carries the models must not re-download them. The skip
// is what makes a replaced rig cheap rather than a second full cold start.
func TestAnImagePreloadedWithComfyUISkipsTheInstall(t *testing.T) {
	g, _ := workflow.Parse([]byte(lifecycleGraph))
	m := &workflow.Manifest{Models: map[string]workflow.Source{
		"base.safetensors": {Repo: "r/b", File: "b.safetensors", Bytes: 10},
		"vae.safetensors":  {Repo: "r/v", File: "v.safetensors", Bytes: 10},
	}}
	bundle, _ := workflow.Resolve(context.Background(), g, nil,
		workflow.ResolveOptions{Manifest: m})
	rt := New(g, bundle, map[string]string{
		"checkpoints/base.safetensors": "https://x.invalid/b", "vae/vae.safetensors": "https://x.invalid/v",
	})
	rt.PollInterval = time.Millisecond

	host := newHost(20, 1)
	host.hasComfy = true
	if err := rt.Bootstrap(context.Background(), host,
		core.ModelSpec{}, core.SizingPlan{}, nil); err != nil {
		t.Fatal(err)
	}
	if host.installed {
		t.Error("comfyui was reinstalled over an image that already had it")
	}
}

// An image with no torch is a configuration problem, not a bad machine: the
// next host runs the same image and fails identically, so falling back would
// buy a second rental to be told the same thing (FR-PROV-05).
func TestAnImageWithoutTorchIsAModelFailure(t *testing.T) {
	rt := New(nil, nil, nil)
	host := &fakeSession{rule: func(cmd string) (string, error) {
		if strings.Contains(cmd, "import torch") {
			return "NOTFOUND\n", nil
		}
		return "", nil
	}}
	err := rt.Bootstrap(context.Background(), host,
		core.ModelSpec{}, core.SizingPlan{}, nil)
	if err == nil {
		t.Fatal("an image with no torch was accepted")
	}
	if !strings.Contains(err.Error(), "torch") {
		t.Errorf("error does not name what is missing: %v", err)
	}
}

func contains[T comparable](haystack []T, needle T) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func (h *hostSim) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ran...)
}
