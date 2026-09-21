// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/claw/comfyui/workflow"
	"go.sovrenix.com/larri/internal/core"
)

// apiGraph is the smallest executable graph that names a model and a latent:
// enough to be resolved, measured and turned into hardware.
const apiGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple",
        "inputs": {"ckpt_name": "sd_xl_base_1.0.safetensors"}},
  "5": {"class_type": "EmptyLatentImage",
        "inputs": {"width": 1024, "height": 1024, "batch_size": 1}},
  "3": {"class_type": "KSampler", "inputs": {"steps": 20, "model": ["4", 0]}}
}`

// uiGraph is the same thing in the serialisation ComfyUI saves by default,
// which /prompt will not accept.
const uiGraph = `{
  "nodes": [
    {"id": 4, "type": "CheckpointLoaderSimple",
     "widgets_values": ["sd_xl_base_1.0.safetensors"]},
    {"id": 5, "type": "EmptyLatentImage", "widgets_values": [1024, 1024, 1]}
  ]
}`

// fixedSizer stands in for Hugging Face, so a plan costs nothing and needs
// nothing. Six and a half gigabytes is the real SDXL checkpoint.
type fixedSizer struct{ bytes uint64 }

func (f fixedSizer) Size(context.Context, string, string, string) (uint64, error) {
	return f.bytes, nil
}

func (f fixedSizer) DownloadURL(s workflow.Source) string {
	return "https://example.invalid/" + s.Repo + "/" + s.File
}

// writeJob lays out a job file and its graph in a directory of their own, and
// returns the path to the job.
func writeJob(t *testing.T, job, graph string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "w.json"), []byte(graph), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "job.yml")
	if err := os.WriteFile(path, []byte(job), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func planJob(t *testing.T, job, graph string, opt claw.Options) (*Kind, *claw.Plan) {
	t.Helper()
	cfg, err := claw.LoadConfig(writeJob(t, job, graph))
	if err != nil {
		t.Fatal(err)
	}
	k := &Kind{sizer: fixedSizer{bytes: 6_938_040_682}}
	p, err := k.Plan(context.Background(), cfg, opt)
	if err != nil {
		t.Fatal(err)
	}
	return k, p
}

// The registry is the only way the daemon reaches this package, so the entry
// has to be there and has to agree with the Kind behind it.
func TestComfyUIIsRegistered(t *testing.T) {
	k, err := claw.Open(Type)
	if err != nil {
		t.Fatal(err)
	}
	if k.Type() != Type {
		t.Errorf("Type() = %q", k.Type())
	}
	if k.Site() != claw.SiteRemote {
		t.Errorf("Site() = %q, want remote: comfyui runs on the rented box", k.Site())
	}
	if _, ok := k.(claw.Remote); !ok {
		t.Error("a remote claw that cannot Collect would destroy the host holding the renders")
	}
}

// Everything §4a asks for, established from a file and a fake listing: what
// the graph needs, how big it is, and what that means in hardware.
func TestThePlanIsDerivedFromTheGraph(t *testing.T) {
	k, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n", apiGraph, claw.Options{})

	if p.ColdStartBytes != 6_938_040_682 {
		t.Errorf("ColdStartBytes = %d, want the measured bundle", p.ColdStartBytes)
	}
	if p.Sizing == nil {
		t.Fatal("Sizing is nil: a measured bundle is a fixed requirement, not one to re-derive per offer")
	}
	// The checkpoint plus the working set for a 1024x1024 latent.
	if p.Sizing.RequiredVRAMBytes <= p.ColdStartBytes {
		t.Errorf("RequiredVRAMBytes = %d, not above the %d of weights",
			p.Sizing.RequiredVRAMBytes, p.ColdStartBytes)
	}
	if p.Criteria.VRAMPerGPUGB < 12 {
		t.Errorf("VRAMPerGPUGB = %d, too small for a 6.5 GB checkpoint", p.Criteria.VRAMPerGPUGB)
	}
	if p.Criteria.DiskGB < 40 {
		t.Errorf("DiskGB = %d: the image, the bundle and the renders all land there", p.Criteria.DiskGB)
	}
	if p.Model.Ref != "w" || p.Model.ServedName != "comfyui" {
		t.Errorf("Model = %+v, want the workflow's own name", p.Model)
	}
	if k.graph.Steps != 20 || k.graph.Image.Width != 1024 {
		t.Errorf("graph = %+v, the latent and the steps were not read", k.graph)
	}
	if got := k.urls["checkpoints/sd_xl_base_1.0.safetensors"]; got == "" {
		t.Error("no source url for the checkpoint")
	}
}

// ComfyUI executes a graph on one device, so two 12 GB cards do not hold a
// 20 GB bundle. The floor is per-GPU and the total may not be the only one met.
func TestTheVRAMFloorIsPerGPU(t *testing.T) {
	_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n", apiGraph, claw.Options{})
	if p.Criteria.VRAMPerGPUGB != p.Criteria.VRAMTotalGB {
		t.Errorf("per-gpu %d, total %d: a bundle that only fits when summed fits nowhere",
			p.Criteria.VRAMPerGPUGB, p.Criteria.VRAMTotalGB)
	}
}

// Renting something smaller than what was asked for is the one direction that
// cannot be undone after the fact (FR-CLAW-04).
func TestThePlanOnlyRaisesTheOperatorsFloors(t *testing.T) {
	base := core.Criteria{VRAMPerGPUGB: 80, VRAMTotalGB: 80, RAMGB: 256, DiskGB: 500}
	_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n", apiGraph,
		claw.Options{Criteria: base})

	if p.Criteria.VRAMPerGPUGB != 80 || p.Criteria.VRAMTotalGB != 80 {
		t.Errorf("vram lowered to %d/%d from 80/80",
			p.Criteria.VRAMPerGPUGB, p.Criteria.VRAMTotalGB)
	}
	if p.Criteria.RAMGB != 256 || p.Criteria.DiskGB != 500 {
		t.Errorf("ram/disk lowered to %d/%d from 256/500", p.Criteria.RAMGB, p.Criteria.DiskGB)
	}
}

// A job file that only works from one working directory breaks the first time
// it runs anywhere else.
func TestTheWorkflowResolvesAgainstTheJobFile(t *testing.T) {
	path := writeJob(t, "type: comfyui\nworkflow: ./w.json\n", apiGraph)
	cfg, err := claw.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// Somewhere ./w.json plainly is not.
	t.Chdir(t.TempDir())

	k := &Kind{sizer: fixedSizer{bytes: 1 << 30}}
	if _, err := k.Plan(context.Background(), cfg, claw.Options{}); err != nil {
		t.Fatalf("the workflow was looked for in the working directory: %v", err)
	}
}

// Caveats are what the operator is told before they spend, so each one has to
// be true of the run in front of them.
func TestCaveatsDescribeThisRun(t *testing.T) {
	always := "comfyui has no server-side credential"

	t.Run("a default run has only the standing two", func(t *testing.T) {
		_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n", apiGraph, claw.Options{})
		if len(p.Caveats) != 2 {
			t.Errorf("caveats = %q, want the two that are always true", p.Caveats)
		}
		if !hasPrefixIn(p.Caveats, always) {
			t.Error("the missing credential was not disclosed")
		}
	})

	t.Run("a ui graph says it cannot be submitted", func(t *testing.T) {
		_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n", uiGraph, claw.Options{})
		if !containsIn(p.Caveats, "ui serialisation") {
			t.Errorf("caveats = %q, want one about the serialisation", p.Caveats)
		}
	})

	t.Run("allowing pickle is said out loud", func(t *testing.T) {
		_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\nallow_pickle: true\n",
			apiGraph, claw.Options{})
		if !containsIn(p.Caveats, "executes code on load") {
			t.Errorf("caveats = %q, want the pickle disclosure", p.Caveats)
		}
	})

	t.Run("naming the default image is not an override", func(t *testing.T) {
		_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\nimage: "+DefaultImage+"\n",
			apiGraph, claw.Options{})
		if containsIn(p.Caveats, "image was overridden") {
			t.Errorf("caveats = %q: the job named the default", p.Caveats)
		}
	})

	t.Run("a different image is", func(t *testing.T) {
		_, p := planJob(t, "type: comfyui\nworkflow: ./w.json\nimage: nvidia/cuda:12.6.0-runtime\n",
			apiGraph, claw.Options{})
		if !containsIn(p.Caveats, "image was overridden") {
			t.Errorf("caveats = %q, want the override disclosed", p.Caveats)
		}
	})
}

// The pins are the piece the server is built from, and a pair that has never
// been run together is what installs 6.5 GB and then fails to import its nodes.
func TestTheServerCarriesTheJobsPins(t *testing.T) {
	k, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n"+
		"comfyui_version: v9.9.9\nimage: nvidia/cuda:12.6.0-runtime\nallow_pickle: true\n",
		apiGraph, claw.Options{})

	r, ok := k.Server(p).(*Runtime)
	if !ok {
		t.Fatal("Server did not build a comfyui runtime")
	}
	if r.ImageRef != "nvidia/cuda:12.6.0-runtime" || r.ComfyUIRef != "v9.9.9" {
		t.Errorf("server = %s / %s, the job's pins were dropped", r.ImageRef, r.ComfyUIRef)
	}
	if !r.AllowPickle {
		t.Error("the operator's pickle opt-in did not reach the host")
	}
	if r.Graph == nil || r.Bundle == nil || len(r.URLs) == 0 {
		t.Error("the server was handed no graph, bundle or sources")
	}
}

// A default job leaves both pins where the project put them, which is the
// pairing that has actually rendered an image.
func TestTheServerDefaultsToThePinnedPair(t *testing.T) {
	k, p := planJob(t, "type: comfyui\nworkflow: ./w.json\n", apiGraph, claw.Options{})
	r := k.Server(p).(*Runtime)
	if r.ImageRef != DefaultImage || r.ComfyUIRef != DefaultComfyUIRef {
		t.Errorf("server = %s / %s, want the pinned pair", r.ImageRef, r.ComfyUIRef)
	}
}

// The error has to name the file, since the operator is looking at a job they
// wrote rather than at a flag they passed.
func TestAJobWithNoWorkflowIsRefused(t *testing.T) {
	cfg, err := claw.LoadConfig(writeJob(t, "type: comfyui\n", apiGraph))
	if err != nil {
		t.Fatal(err)
	}
	k := &Kind{sizer: fixedSizer{bytes: 1 << 30}}
	_, err = k.Plan(context.Background(), cfg, claw.Options{})
	if err == nil {
		t.Fatal("a job naming no workflow planned successfully")
	}
	if !strings.Contains(err.Error(), "workflow") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

func containsIn(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func hasPrefixIn(lines []string, want string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, want) {
			return true
		}
	}
	return false
}
