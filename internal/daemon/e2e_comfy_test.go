//go:build e2e

// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// One complete ComfyUI workflow against real hardware.
//
// Build-tagged and doubly gated, because this one rents a GPU:
//
//	VASTAI_API_KEY=... HF_TOKEN=... LARRI_E2E_SPEND=yes \
//	  LARRI_E2E_WORKFLOW=./testdata/sdxl.json \
//	  go test -tags e2e -v -timeout 90m -run TestE2EComfyWorkflow ./internal/daemon/
//
// Teardown is registered before anything can fail, runs on panic, and verifies
// absence rather than trusting the destroy call.
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/comfy"
	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider"
	_ "go.sovrenix.com/larri/internal/provider/runpod"
	_ "go.sovrenix.com/larri/internal/provider/vastai"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/workflow"
)

// defaultE2EGraph is a minimal SDXL text-to-image workflow.
//
// Deliberately small and deliberately ungated: the run is here to test LARRI,
// not an access-approval queue, and not the operator's patience with a 24 GB
// FLUX download. Override it with LARRI_E2E_WORKFLOW to exercise a real one.
const defaultE2EGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple",
        "inputs": {"ckpt_name": "sd_xl_base_1.0.safetensors"}},
  "6": {"class_type": "CLIPTextEncode",
        "inputs": {"text": "a photograph of a lobster on a beach, golden hour",
                   "clip": ["4", 1]}},
  "7": {"class_type": "CLIPTextEncode",
        "inputs": {"text": "blurry, watermark", "clip": ["4", 1]}},
  "5": {"class_type": "EmptyLatentImage",
        "inputs": {"width": 1024, "height": 1024, "batch_size": 1}},
  "3": {"class_type": "KSampler",
        "inputs": {"seed": 1, "steps": 20, "cfg": 8.0,
                   "sampler_name": "euler", "scheduler": "normal", "denoise": 1.0,
                   "model": ["4", 0], "positive": ["6", 0], "negative": ["7", 0],
                   "latent_image": ["5", 0]}},
  "8": {"class_type": "VAEDecode", "inputs": {"samples": ["3", 0], "vae": ["4", 2]}},
  "9": {"class_type": "SaveImage", "inputs": {"filename_prefix": "larri", "images": ["8", 0]}}
}`

func TestE2EComfyWorkflow(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to rent real hardware")
	}

	raw := []byte(defaultE2EGraph)
	if path := os.Getenv("LARRI_E2E_WORKFLOW"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read workflow: %v", err)
		}
		raw = b
	}
	graph, err := workflow.Parse(raw)
	if err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	if !graph.Format.Executable() {
		t.Fatal("the e2e workflow must be in the api serialisation; /prompt accepts no other")
	}
	t.Logf("workflow: %d nodes, %dx%d, %d steps",
		graph.Nodes, graph.Image.Width, graph.Image.Height, graph.Steps)

	// ---- everything before the money --------------------------------------
	//
	// Resolution is a real network call against Hugging Face. It is inside the
	// paid test rather than the free one precisely because it reaches out, and
	// it runs BEFORE the create call so that a bad manifest, a moved
	// repository, or a token that cannot read a gated repo costs nothing.
	hf := secret.New(os.Getenv("HF_TOKEN"))
	sizer := workflow.NewHFSizer(hf)

	var manifest *workflow.Manifest
	if path := os.Getenv("LARRI_E2E_MODELS"); path != "" {
		if manifest, err = workflow.LoadManifest(path); err != nil {
			t.Fatalf("load manifest: %v", err)
		}
	}
	rctx, cancelResolve := context.WithTimeout(context.Background(), 2*time.Minute)
	bundle, err := workflow.Resolve(rctx, graph, sizer,
		workflow.ResolveOptions{Manifest: manifest})
	cancelResolve()
	if err != nil {
		t.Fatalf("resolve models: %v", err)
	}
	t.Logf("bundle: %d files, %s", len(bundle.Items), sizing.HumanBytes(bundle.TotalBytes))
	urls := make(map[string]string, len(bundle.Items))
	for _, it := range bundle.Items {
		urls[it.Asset.Name] = sizer.DownloadURL(it.Source)
		t.Logf("  %-16s %-40s %s", it.Asset.Kind, it.Asset.Name,
			sizing.HumanBytes(it.Bytes))
	}

	plan, err := sizing.PlanDiffusion(sizing.DiffusionRequest{
		WeightBytes: bundle.TotalBytes, Pixels: bundle.Image.Pixels(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("vram: ~%s", sizing.HumanBytes(plan.RequiredVRAMBytes))

	// ---- the rig ----------------------------------------------------------
	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p, provName := e2eProvider(t)
	provider.Report(p,
		func(e error) { t.Errorf("SHAPE DRIFT: %v", e) },
		func(m string) { t.Logf("notice: %s", m) })
	t.Logf("provider: %s", provName)

	events := make(chan Event, 256)
	done := make(chan struct{})
	go func() {
		for e := range events {
			mark := " "
			if e.Warning {
				mark = "!"
			}
			t.Logf("%s %-10s %s", mark, e.Phase, e.Message)
		}
		close(done)
	}()

	labelKey, keySrc, err := config.ResolveLabelKey(os.Getenv, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := config.LabelSealer(labelKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("label key: %s", keySrc)

	maxPrice := 1.20
	o := &Orchestrator{
		Store: st, Provider: p,
		Policy:      rank.DefaultPolicy(),
		Deadline:    75 * time.Minute,
		Events:      events,
		LabelSealer: sealer,
		// The cheap tier's host failure rate is well above what three
		// attempts covers, and a ComfyUI cold start is long enough that
		// losing one to a dead container is expensive.
		MaxHostAttempts: 6,
		OutputBudget:    10 * time.Minute,
	}

	// Registered before anything can fail. A create that returns an error may
	// still have produced an instance, so the sweep is by label rather than by
	// whatever the happy path recorded.
	t.Cleanup(func() {
		close(events)
		<-done
		sweepOrphans(t, p, st)
	})

	outDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()

	sess, err := o.ComfyUp(ctx, ComfyRequest{
		Name:   "e2e-comfy",
		Graph:  graph,
		Bundle: bundle,
		URLs:   urls,
		Criteria: core.Criteria{
			MaxPriceHr: maxPrice, MinReliability: 0.90,
			// The bundle is downloaded at the rig's hourly rate, so a slow
			// link is a cost problem rather than a preference (§4b).
			MinNetMbps: 200,
		},
		LocalPort:    0, // kernel-chosen, so a stray local 8188 cannot fail the run
		HFToken:      hf,
		OutputDir:    outDir,
		SessionHours: 1,
	})
	if err != nil {
		t.Fatalf("bring-up: %v", err)
	}
	rig := sess.Live.Rig
	t.Logf("READY  rig=%s instance=%s %s $%.3f/hr",
		rig.ID, rig.Instance.InstanceID, rig.Offer.GPUModel, rig.Offer.PriceHr)
	t.Logf("browser: %s", sess.URL)
	for _, c := range sess.Caveats {
		t.Logf("caveat: %s", c)
	}

	// Reaching READY already proves a render: readiness submits the
	// operator's own graph and waits for a file. This second one proves the
	// rig keeps working — the weights stayed resident, the queue drains
	// twice, and the endpoint survives its own first use.
	t.Run("render", func(t *testing.T) {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		entry, err := sess.Render(rctx)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		files := entry.Files()
		if len(files) == 0 {
			t.Fatal("the graph completed and produced no file")
		}
		for _, f := range files {
			t.Logf("rendered %s/%s", f.Subfolder, f.Filename)
		}
	})

	// The rig has to survive being used, so the check that matters at the end
	// is that it is still answering rather than that it once did.
	t.Run("still serving", func(t *testing.T) {
		qctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := sess.Client.Reachable(qctx); err != nil {
			t.Errorf("the rig stopped answering after a render: %v", err)
		}
	})

	// ---- collect, then destroy, then confirm absence ----------------------
	term := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		At: time.Now().UTC(), Summary: "e2e complete",
	}
	// A fresh context: the collection and the destroy both have to run even
	// if the one above has expired, and the destroy especially.
	tctx, tcancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer tcancel()

	sync, derr := o.ComfyDown(tctx, sess, term)
	if sync == nil {
		t.Error("no renders were collected before teardown; they are gone with the host")
	} else {
		t.Logf("collected: %s (%s) into %s",
			sync.Summary(), sizing.HumanBytes(sync.Bytes), sync.Dir)
		if !sync.Complete() {
			t.Errorf("collection incomplete: %v", sync.Failed)
		}
		if sync.Count() == 0 {
			t.Error("the session rendered at least twice and collected nothing")
		}
	}
	if derr != nil {
		t.Fatalf("teardown: %v", derr)
	}

	// The images must actually be on local disk, not merely reported as such.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	var images int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.Size() == 0 {
			t.Errorf("%s is empty", e.Name())
			continue
		}
		images++
		t.Logf("saved %s (%s)", filepath.Base(e.Name()), sizing.HumanBytes(uint64(info.Size())))
	}
	if images == 0 {
		t.Error("no image reached local disk")
	}

	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
	if rig.End == nil {
		t.Fatal("no termination record: a destroyed rig must be able to say why it went")
	}
	t.Logf("spent $%.4f over %s", rig.End.Cost.TotalUSD, rig.End.Cost.Ran.Round(time.Second))

	// Absence, not a 200 from a delete endpoint (FR-DEL-03).
	inst, gerr := p.Get(tctx, rig.Instance.InstanceID)
	if gerr == nil && inst != nil {
		t.Fatalf("INSTANCE %s STILL EXISTS AND IS BILLING", rig.Instance.InstanceID)
	}
}

// A ComfyUI rig must never be mistaken for something that serves completions.
// Cheap to assert and it runs in the paid suite because that is where a real
// workload object exists.
func TestE2EComfyIsNotAnOpenAIEndpoint(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to rent real hardware")
	}
	rt := comfy.New(nil, nil, nil)
	if runtime.RequireOpenAI(rt) {
		t.Fatal("the comfyui workload claims the /v1 contract")
	}
}
