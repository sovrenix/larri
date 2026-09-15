// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/comfy"
	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/workflow"
)

// sdxlGraph is a stock text-to-image workflow: a checkpoint, a LoRA, a VAE,
// and a 1024x1024 latent.
const sdxlGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple",
        "inputs": {"ckpt_name": "base.safetensors"}},
  "10": {"class_type": "LoraLoader", "inputs": {"lora_name": "style.safetensors"}},
  "11": {"class_type": "VAELoader", "inputs": {"vae_name": "vae.safetensors"}},
  "5": {"class_type": "EmptyLatentImage",
        "inputs": {"width": 1024, "height": 1024, "batch_size": 1}},
  "3": {"class_type": "KSampler", "inputs": {"steps": 25}},
  "9": {"class_type": "SaveImage", "inputs": {"images": ["8", 0]}}
}`

// comfyBundle parses the graph and measures it without touching the network.
func comfyBundle(t *testing.T) (*workflow.Graph, *workflow.Bundle) {
	t.Helper()
	g, err := workflow.Parse([]byte(sdxlGraph))
	if err != nil {
		t.Fatal(err)
	}
	m := &workflow.Manifest{Models: map[string]workflow.Source{
		"base.safetensors":  {Repo: "r/base", File: "base.safetensors", Bytes: 6_940_000_000},
		"style.safetensors": {Repo: "r/lora", File: "style.safetensors", Bytes: 200_000_000},
		"vae.safetensors":   {Repo: "r/vae", File: "vae.safetensors", Bytes: 335_000_000},
	}}
	b, err := workflow.Resolve(context.Background(), g, nil,
		workflow.ResolveOptions{Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	return g, b
}

// The whole feature turns on this: a JSON file and a repository listing are
// enough to say what hardware to rent, and every one of these floors is free
// to establish and expensive to discover afterwards (§4a).
func TestCriteriaComeFromTheWorkflow(t *testing.T) {
	_, b := comfyBundle(t)
	plan, err := sizing.PlanDiffusion(sizing.DiffusionRequest{
		WeightBytes: b.TotalBytes, Pixels: b.Image.Pixels(),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := CriteriaFor(core.Criteria{}, plan, b)

	need := int(plan.RequiredVRAMBytes / sizing.GiB)
	if c.VRAMTotalGB < need {
		t.Errorf("vram total = %d GB, want at least %d", c.VRAMTotalGB, need)
	}
	// The one that matters: ComfyUI runs a graph on one device, so two small
	// cards do not add up to one large one.
	if c.VRAMPerGPUGB < need {
		t.Errorf("vram per gpu = %d GB, want at least %d — two 12 GB cards "+
			"do not hold a 20 GB bundle", c.VRAMPerGPUGB, need)
	}
	if c.RAMGB < 16 {
		t.Errorf("ram = %d GB; comfyui offloads to host memory and needs somewhere to put it",
			c.RAMGB)
	}
	// Disk holds the image, the bundle, and the renders.
	if wantDisk := int(b.TotalBytes/sizing.GiB) * 2; c.DiskGB < wantDisk {
		t.Errorf("disk = %d GB, want at least %d", c.DiskGB, wantDisk)
	}
}

// An operator's own floors are a minimum, not a suggestion: asking for more
// than the workflow needs must not be quietly reduced to what it needs.
func TestOperatorFloorsAreNotLowered(t *testing.T) {
	_, b := comfyBundle(t)
	plan, _ := sizing.PlanDiffusion(sizing.DiffusionRequest{
		WeightBytes: b.TotalBytes, Pixels: b.Image.Pixels(),
	})
	base := core.Criteria{VRAMPerGPUGB: 80, VRAMTotalGB: 80, RAMGB: 256, DiskGB: 500}
	c := CriteriaFor(base, plan, b)
	if c.VRAMPerGPUGB != 80 || c.RAMGB != 256 || c.DiskGB != 500 {
		t.Errorf("criteria = %+v, want the operator's larger floors kept", c)
	}
}

// comfyOrch builds an orchestrator sized against a ComfyUI bundle, with a
// market whose offers differ in the way that decides this selection.
func comfyOrch(t *testing.T, offers []core.Offer) *Orchestrator {
	t.Helper()
	_, b := comfyBundle(t)
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	g, _ := workflow.Parse([]byte(sdxlGraph))
	o := &Orchestrator{
		Store:    st,
		Provider: pfake.New("fake", offers, pfake.Behaviour{}),
		Runtime:  comfy.New(g, b, nil),
		Policy:   rank.DefaultPolicy(),
		Deadline: time.Minute,
		Planner: func(context.Context, UpRequest) (core.SizingPlan, error) {
			return sizing.PlanDiffusion(sizing.DiffusionRequest{
				WeightBytes: b.TotalBytes, Pixels: b.Image.Pixels(),
			})
		},
	}
	return o
}

// §4b: the bundle is downloaded at the rig's hourly rate, so the cheapest
// listing is routinely the dearest way to get a working endpoint. With 7.5 GB
// to fetch, a slow link costs more than a dearer card with a fast one.
func TestSelectionRanksSetupTimeAlongsidePrice(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "slow", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.30, Reliability: 0.97,
			NetDownMbps: 50, MachineID: "m-slow"},
		{Provider: "fake", OfferID: "fast", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.97,
			NetDownMbps: 2000, MachineID: "m-fast"},
	}
	o := comfyOrch(t, market)
	o.Policy.SessionHours = 1

	sv, err := o.Offers(context.Background(), UpRequest{
		Model: comfy.ModelSpecFor("sdxl", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sv.Selection.Selected == nil {
		t.Fatal("nothing was selected")
	}
	if got := sv.Selection.Selected.Offer.OfferID; got != "fast" {
		t.Errorf("selected %q at the lower hourly rate; the download is billed "+
			"at that rate too, and 7.5 GB over 50 Mbps is twenty minutes of it", got)
	}
}

// Over a long session the hourly rate dominates again, and the cheap host
// becomes the right answer. Ranking must follow the session length rather
// than hard-coding a preference for fast links.
func TestALongSessionPrefersTheCheaperHourlyRate(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "slow", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.30, Reliability: 0.97,
			NetDownMbps: 50, MachineID: "m-slow"},
		{Provider: "fake", OfferID: "fast", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.97,
			NetDownMbps: 2000, MachineID: "m-fast"},
	}
	o := comfyOrch(t, market)
	o.Policy.SessionHours = 12

	sv, err := o.Offers(context.Background(), UpRequest{
		Model: comfy.ModelSpecFor("sdxl", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sv.Selection.Selected.Offer.OfferID; got != "slow" {
		t.Errorf("selected %q over a twelve-hour session; the download is "+
			"amortised and the hourly rate dominates", got)
	}
}

// A card that cannot hold the bundle is excluded by the planner's own
// arithmetic rather than by the transformer path, which has no facts to work
// from here.
func TestOffersTooSmallForTheBundleAreExcluded(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "tiny", GPUModel: "GTX 1650", GPUCount: 1,
			VRAMPerGPUGB: 4, PriceHr: 0.05, Reliability: 0.97, MachineID: "m-tiny"},
		{Provider: "fake", OfferID: "ok", GPUModel: "RTX 4090", GPUCount: 1,
			VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.97, MachineID: "m-ok"},
	}
	o := comfyOrch(t, market)
	sv, err := o.Offers(context.Background(), UpRequest{
		Model: comfy.ModelSpecFor("sdxl", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sv.Selection.Selected.Offer.OfferID != "ok" {
		t.Errorf("selected %q, which cannot hold the bundle",
			sv.Selection.Selected.Offer.OfferID)
	}
	var sawReason bool
	for _, c := range sv.Selection.Excluded() {
		if c.Offer.OfferID == "tiny" && c.Reason == rank.ReasonVRAM {
			sawReason = true
		}
	}
	if !sawReason {
		t.Error("the undersized card was not excluded for VRAM, with a reason attached")
	}
}

// A market with nothing large enough must fail before spending, and say what
// it could not satisfy rather than running transformer analysis it has no
// facts for.
func TestAnImpossibleBundleIsRejectedBeforeSpending(t *testing.T) {
	market := []core.Offer{
		{Provider: "fake", OfferID: "tiny", GPUModel: "GTX 1650", GPUCount: 1,
			VRAMPerGPUGB: 4, PriceHr: 0.05, Reliability: 0.97, MachineID: "m-tiny"},
	}
	o := comfyOrch(t, market)
	_, err := o.Offers(context.Background(), UpRequest{
		Model: comfy.ModelSpecFor("sdxl", nil),
	})
	if err == nil {
		t.Fatal("a market with no card large enough was accepted")
	}
}

// State is money (§4). A rig that cannot hand over its images is still a rig
// that must be destroyed, and the destroy must not be conditional on the
// retrieval succeeding.
func TestTeardownProceedsWhenOutputsCannotBeCollected(t *testing.T) {
	o := comfyOrch(t, []core.Offer{{
		Provider: "fake", OfferID: "a", GPUModel: "RTX 4090", GPUCount: 1,
		VRAMPerGPUGB: 24, PriceHr: 0.40, Reliability: 0.97, MachineID: "m",
	}})
	rig, err := o.Up(context.Background(), UpRequest{
		Model: comfy.ModelSpecFor("sdxl", nil), DiskGB: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A session whose SSH is gone: collection is impossible by construction.
	s := &ComfySession{
		Live:      &Live{Rig: rig},
		OutputDir: t.TempDir(),
		StartedAt: time.Now(),
	}
	term := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		At: time.Now().UTC(), Summary: "test",
	}
	if _, err := o.ComfyDown(context.Background(), s, term); err != nil {
		t.Fatalf("teardown refused to proceed without its images: %v", err)
	}
	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
	if rig.End == nil {
		t.Error("no termination record; a destroyed rig must say why it went")
	}
}
