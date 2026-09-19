//go:build e2e

// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// One complete claw through the rental lifecycle, against real hardware.
//
// Build-tagged and doubly gated, because this one rents a GPU:
//
//	VASTAI_API_KEY=... HF_TOKEN=... LARRI_E2E_SPEND=yes \
//	  go test -tags e2e -v -timeout 90m -run TestE2EClawComfyUI ./internal/daemon/
//
// It drives the generic path — claw.Open, Kind.Plan, ClawUp, ClawDown — with
// ComfyUI as the type, so what it proves is the layer as much as the
// application. Point it at your own job file with LARRI_E2E_JOB.
//
// Teardown is registered before anything can fail, runs on panic, and verifies
// absence rather than trusting the destroy call.
package daemon

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/claw/comfyui"
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
)

// defaultE2EGraph is a minimal SDXL text-to-image workflow.
//
// Deliberately small and deliberately ungated: the run is here to test LARRI,
// not an access-approval queue, and not the operator's patience with a 24 GB
// FLUX download. Override the whole job with LARRI_E2E_JOB to exercise a real
// one.
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

// e2eJob writes the default job beside its graph, or returns the operator's.
func e2eJob(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LARRI_E2E_JOB"); path != "" {
		return path
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "w.json"), []byte(defaultE2EGraph), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "job.yml")
	if err := os.WriteFile(path, []byte("type: comfyui\nworkflow: ./w.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestE2EClawComfyUI(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to rent real hardware")
	}

	// ---- everything before the money --------------------------------------
	//
	// All of it is Kind.Plan, which is the point of the split: reading the
	// job, resolving each model against the live listing, measuring it and
	// turning that into hardware happens here, locally, before any create
	// call. A moved repository or a token that cannot read a gated repo costs
	// nothing to discover (§4a).
	cfg, err := claw.LoadConfig(e2eJob(t))
	if err != nil {
		t.Fatal(err)
	}
	kind, err := claw.Open(cfg.Type)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("claw: %s (%s) from %s", kind.Type(), kind.Site(), cfg.Path)

	hf := secret.New(os.Getenv("HF_TOKEN"))
	outDir := t.TempDir()

	pctx, cancelPlan := context.WithTimeout(context.Background(), 3*time.Minute)
	plan, err := kind.Plan(pctx, cfg, claw.Options{
		Criteria: core.Criteria{
			MaxPriceHr: 1.20, MinReliability: 0.90,
			// Whatever the claw must download is billed at the rig's hourly
			// rate, so a slow link is a cost problem rather than a preference
			// (§4b).
			MinNetMbps: 200,
		},
		OutputDir: outDir, HFToken: hf, SessionHours: 1,
	})
	cancelPlan()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, line := range plan.Summary {
		t.Logf("plan: %s", line)
	}
	for _, c := range plan.Caveats {
		t.Logf("caveat: %s", c)
	}
	if plan.Criteria.MaxPriceHr != 1.20 || plan.Criteria.MinNetMbps != 200 {
		t.Fatalf("the plan lowered the operator's floors: %+v", plan.Criteria)
	}

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

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Minute)
	defer cancel()

	sess, err := o.ClawUp(ctx, ClawRequest{
		Kind: kind, Plan: plan,
		LocalPort: 0, // kernel-chosen, so a stray local 8188 cannot fail the run
		HFToken:   hf,
		OutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("bring-up: %v", err)
	}
	rig := sess.Live.Rig
	t.Logf("READY  rig=%s instance=%s %s $%.3f/hr",
		rig.ID, rig.Instance.InstanceID, rig.Offer.GPUModel, rig.Offer.PriceHr)
	t.Logf("browser: %s", sess.URL)

	// A claw that is not an inference engine must never be reachable as one.
	if runtime.RequireOpenAI(o.Runtime) {
		t.Error("the comfyui workload claims the /v1 contract")
	}

	client := e2eComfyClient(t, sess)

	// Reaching READY already proves a render: readiness submits the operator's
	// own graph and waits for a file. This second one proves the rig keeps
	// working — the weights stayed resident, the queue drains twice, and the
	// endpoint survives its own first use.
	t.Run("render", func(t *testing.T) {
		rctx, rcancel := context.WithTimeout(ctx, 15*time.Minute)
		defer rcancel()

		sub, err := client.Submit(rctx, json.RawMessage(readGraph(t, cfg)), "e2e")
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		entry, err := client.Await(rctx, sub.PromptID, 3*time.Second)
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
		qctx, qcancel := context.WithTimeout(ctx, time.Minute)
		defer qcancel()
		if err := client.Reachable(qctx); err != nil {
			t.Errorf("the rig stopped answering after a render: %v", err)
		}
	})

	// ---- collect, then destroy, then confirm absence ----------------------
	term := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		At: time.Now().UTC(), Summary: "e2e complete",
	}
	// A fresh context: the collection and the destroy both have to run even if
	// the one above has expired, and the destroy especially.
	tctx, tcancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer tcancel()

	res, derr := o.ClawDown(tctx, sess, term)
	if res == nil {
		t.Error("no renders were collected before teardown; they are gone with the host")
	} else {
		t.Logf("collected: %s (%s) into %s",
			res.Summary(), sizing.HumanBytes(res.Bytes), res.Dir)
		if !res.Complete() {
			t.Errorf("collection incomplete: %v", res.Failed)
		}
		if res.Count() == 0 {
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

// e2eComfyClient speaks to the session the way the operator's browser does:
// through the fixed local port, so the forward, the proxy and the credential
// substitution are all on the path being tested.
func e2eComfyClient(t *testing.T, s *ClawSession) *comfyui.Client {
	t.Helper()
	u, err := url.Parse(s.Endpoint())
	if err != nil {
		t.Fatalf("endpoint %q: %v", s.Endpoint(), err)
	}
	return &comfyui.Client{Addr: u.Host, Token: s.Live.ClientToken.Reveal()}
}

// readGraph re-reads the job's workflow for the second render. The Kind holds
// its own copy and does not lend it out, which is right: what it parsed is an
// input to a plan rather than a handle for callers to reuse.
func readGraph(t *testing.T, cfg *claw.Config) []byte {
	t.Helper()
	var job comfyui.Config
	if err := cfg.Decode(&job); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.Resolve(job.Workflow))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
