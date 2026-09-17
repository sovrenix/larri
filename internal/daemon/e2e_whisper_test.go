//go:build e2e

// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// One complete *local* claw against real hardware.
//
// Build-tagged and doubly gated, because this one rents a GPU:
//
//	RUNPOD_API_KEY=... LARRI_E2E_SPEND=yes \
//	  go test -tags e2e -v -timeout 60m -run TestE2EClawWhisper ./internal/daemon/
//
// The companion to TestE2EClawComfyUI, and deliberately the other site. That
// one proves a remote claw: the application on the box, results collected
// before the destroy. This one proves the local site, where nothing is
// produced on the host and what has to be got right is the wiring — a
// credential a client can actually authenticate with, a record of what was
// changed, a probe that says the client arrived, and a revert that runs before
// the instance goes.
//
// Teardown is registered before anything can fail, runs on panic, and verifies
// absence rather than trusting the destroy call.
package daemon

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/claw/whisper"
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
	"go.sovrenix.com/larri/internal/wire"
)

// e2eWhisperClient is the name the wiring is keyed on. It stands in for
// whatever the operator calls their transcription application, which is all a
// guided writer ever knows about it.
const e2eWhisperClient = "e2e-transcriber"

// defaultE2EWhisperJob is small on purpose. The run is here to test LARRI, not
// to benchmark a model: large-v3 is the accurate one and distil-large-v3 is
// most of the way there for half the download, which is half the billed
// minutes before anything works.
const defaultE2EWhisperJob = `type: whisper
model: distil-large-v3
compute_type: float16
language: en
clients: [` + e2eWhisperClient + `]
`

// e2eWhisperJob writes the default job, or returns the operator's.
func e2eWhisperJob(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LARRI_E2E_WHISPER_JOB"); path != "" {
		return path
	}
	path := filepath.Join(t.TempDir(), "transcribe.yml")
	if err := os.WriteFile(path, []byte(defaultE2EWhisperJob), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestE2EClawWhisper(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to rent real hardware")
	}

	// ---- everything before the money --------------------------------------
	//
	// One listing fetch. A model name that does not resolve, or a gated
	// repository this token cannot read, ends the run here for nothing (§4a).
	cfg, err := claw.LoadConfig(e2eWhisperJob(t))
	if err != nil {
		t.Fatal(err)
	}
	kind, err := claw.Open(cfg.Type)
	if err != nil {
		t.Fatal(err)
	}
	if kind.Site() != claw.SiteLocal {
		t.Fatalf("site = %q: this run exists to exercise the local one", kind.Site())
	}

	hf := secret.New(os.Getenv("HF_TOKEN"))
	pctx, cancelPlan := context.WithTimeout(context.Background(), 3*time.Minute)
	plan, err := kind.Plan(pctx, cfg, claw.Options{
		Criteria: core.Criteria{
			MaxPriceHr: 0.60, MinReliability: 0.90, MinNetMbps: 200,
			// The market has little in stock at the small end, and a
			// transcription rig wants a small card. A refused create falls
			// back rather than failing the run.
			AllowLowStock: true,
		},
		HFToken: hf, SessionHours: 1,
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
	if plan.Sizing == nil {
		t.Fatal("a speech model was sized by the transformer path, which cannot mean anything here")
	}
	t.Logf("vram: ~%s, download %s",
		sizing.HumanBytes(plan.Sizing.RequiredVRAMBytes),
		sizing.HumanBytes(plan.ColdStartBytes))

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

	// A credential of this test's own, so the run cannot depend on — or
	// disturb — whatever is stored for this machine.
	base, err := secret.Generate(32)
	if err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{
		Store: st, Provider: p,
		Policy:          rank.DefaultPolicy(),
		Deadline:        45 * time.Minute,
		Events:          events,
		LabelSealer:     sealer,
		ClientToken:     base,
		MaxHostAttempts: 6,
	}

	// Registered before anything can fail. A create that returns an error may
	// still have produced an instance, so the sweep is by label rather than by
	// whatever the happy path recorded.
	t.Cleanup(func() {
		close(events)
		<-done
		sweepOrphans(t, p, st)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	sess, err := o.ClawUp(ctx, ClawRequest{
		Kind: kind, Plan: plan,
		LocalPort: 0, // kernel-chosen, so a stray local 8188 cannot fail the run
		HFToken:   hf,
		// No OutputDir, and that is the site's whole point: a local claw
		// produces nothing on the host, so there is nothing to collect.
	})
	if err != nil {
		t.Fatalf("bring-up: %v", err)
	}
	rig := sess.Live.Rig
	t.Logf("READY  rig=%s instance=%s %s $%.3f/hr",
		rig.ID, rig.Instance.InstanceID, rig.Offer.GPUModel, rig.Offer.PriceHr)
	t.Logf("endpoint: %s", sess.Endpoint())

	// A transcription server must never be reachable as a chat endpoint.
	if runtime.RequireOpenAI(o.Runtime) {
		t.Error("the whisper workload claims the /v1 chat contract")
	}

	// ---- the wiring -------------------------------------------------------
	//
	// The half this run exists for. Everything below is about the operator's
	// machine rather than the rented one.
	t.Run("wiring", func(t *testing.T) {
		if len(sess.Wiring) != 1 {
			t.Fatalf("recorded %d wired clients, want 1: %+v", len(sess.Wiring), sess.Wiring)
		}
		rec := sess.Wiring[0]
		if rec.Client != e2eWhisperClient {
			t.Errorf("wired %q", rec.Client)
		}
		if rec.Tier != string(wire.TierGuided) {
			t.Errorf("tier = %q, want C: nothing was written, so nothing may claim it was", rec.Tier)
		}
		if rec.Path != "" || rec.BackupPath != "" {
			t.Errorf("a guided client recorded a file it never touched: %+v", rec)
		}
		// Nothing has connected yet, and the record must say so rather than
		// claim a verification it has no evidence for.
		if rec.Verified {
			t.Error("the client was recorded as verified before it had sent anything")
		}
		// Persisted, because what has to be undone must survive this process
		// dying (§10.2 step 4).
		if rig.Wiring == nil || len(rig.Wiring) != 1 {
			t.Errorf("the wiring was not persisted on the rig: %+v", rig.Wiring)
		}
	})

	// ---- the round trip ---------------------------------------------------
	//
	// Through the fixed local port with the client's *own* derived credential,
	// which is what an operator pasting the printed values would present. That
	// exercises the forward, the proxy, the credential substitution and the
	// server in one call — and it is what makes the probe below meaningful.
	client := e2eWhisperClientFor(t, sess, base)

	t.Run("transcribe", func(t *testing.T) {
		rctx, rcancel := context.WithTimeout(ctx, 5*time.Minute)
		defer rcancel()

		// A catalogue of what the server can fetch, not what it loaded — which
		// a live run established the awkward way. Logged, never asserted on.
		models, err := client.Models(rctx)
		if err != nil {
			t.Fatalf("models: %v", err)
		}
		t.Logf("server offers %d models", len(models))

		text, err := client.Transcribe(rctx, whisper.ReadyClip(), "e2e.wav", "Systran/faster-distil-whisper-large-v3")
		if err != nil {
			t.Fatalf("transcribe: %v", err)
		}
		// What it heard in noise is not the assertion. That it ran the model
		// and answered is.
		t.Logf("transcribed: %q", text)
	})

	t.Run("the probe sees the client that arrived", func(t *testing.T) {
		probe := wire.ProxyProber(sess.Live.proxy)
		if probe == nil {
			t.Fatal("no prober: verification would be impossible for a guided client")
		}
		ok, err := probe(e2eWhisperClient)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !ok {
			t.Error("the client sent a transcription and the proxy did not record it arriving")
		}
		// A client that was never wired must not be claimed either.
		if seen, _ := probe("never-configured"); seen {
			t.Error("an unwired client was reported as having arrived")
		}
	})

	// ---- revert, then destroy, then confirm absence ------------------------
	term := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		At: time.Now().UTC(), Summary: "e2e complete",
	}
	// A fresh context: the revert and the destroy both have to run even if the
	// one above has expired, and the destroy especially.
	tctx, tcancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer tcancel()

	res, derr := o.ClawDown(tctx, sess, term)
	if res != nil {
		t.Errorf("a local claw collected %d results from a host that produces none", res.Count())
	}
	if derr != nil {
		t.Fatalf("teardown: %v", derr)
	}

	if rig.State != core.StateDestroyed {
		t.Errorf("state = %s, want DESTROYED", rig.State)
	}
	if rig.End == nil {
		t.Fatal("no termination record: a destroyed rig must be able to say why it went")
	}
	t.Logf("spent $%.4f over %s", rig.End.Cost.TotalUSD, rig.End.Cost.Ran.Round(time.Second))

	// The endpoint must stop answering once the rig is gone. A local port that
	// still accepts requests after a teardown is how a client goes on
	// believing it is configured.
	pctx2, pcancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer pcancel()
	if err := client.Reachable(pctx2); err == nil {
		t.Error("the endpoint still answers after the rig was destroyed")
	}

	// Absence, not a 200 from a delete endpoint (FR-DEL-03).
	inst, gerr := p.Get(tctx, rig.Instance.InstanceID)
	if gerr == nil && inst != nil {
		t.Fatalf("INSTANCE %s STILL EXISTS AND IS BILLING", rig.Instance.InstanceID)
	}
}

// e2eWhisperClientFor speaks to the session the way the operator's application
// would: the fixed local port, and the credential derived for this client
// rather than the rig's own.
func e2eWhisperClientFor(t *testing.T, s *ClawSession, base secret.Secret) *whisper.Client {
	t.Helper()
	u, err := url.Parse(s.Endpoint())
	if err != nil {
		t.Fatalf("endpoint %q: %v", s.Endpoint(), err)
	}
	return &whisper.Client{
		Addr:  u.Host,
		Token: clientToken(base, e2eWhisperClient).Reveal(),
	}
}
