//go:build e2e

// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// The full lifecycle against real hardware.
//
// Build-tagged and doubly gated, because this one rents a GPU:
//
//	VASTAI_API_KEY=... HF_TOKEN=... LARRI_E2E_SPEND=yes \
//	  go test -tags e2e -v -timeout 40m ./internal/daemon/
//
// Teardown is registered before anything can fail and runs on panic, and it
// verifies absence rather than trusting the destroy call. If it cannot
// confirm, it says so loudly with the instance ID.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider/vastai"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/runtime/vllm"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/state"
)

func TestE2ERentServeDestroy(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to rent real hardware")
	}
	vastKey := os.Getenv("VASTAI_API_KEY")
	if vastKey == "" {
		t.Skip("VASTAI_API_KEY not set")
	}
	// A small ungated model, so the run tests LARRI rather than an access
	// approval queue.
	model := os.Getenv("LARRI_E2E_MODEL")
	if model == "" {
		model = "Qwen/Qwen2.5-1.5B-Instruct"
	}
	maxPrice := 0.40

	st, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p := vastai.New(secret.New(vastKey))
	p.OnNotice = func(m string) { t.Logf("notice: %s", m) }
	p.OnDrift = func(e error) { t.Errorf("SHAPE DRIFT: %v", e) }

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

	o := &Orchestrator{
		Store: st, Provider: p, Runtime: vllm.New(),
		Resolver: sizing.NewHFResolver(secret.New(os.Getenv("HF_TOKEN"))),
		Policy:   rank.DefaultPolicy(),
		Deadline: 35 * time.Minute,
		Events:   events,
	}

	// Registered before anything can fail. A create that returns an error may
	// still have produced an instance, so the sweep is by label rather than
	// by whatever the happy path recorded.
	t.Cleanup(func() {
		close(events)
		<-done
		sweepOrphans(t, p, st)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()

	live, err := o.UpAndServe(ctx, UpRequest{
		Criteria: core.Criteria{MaxPriceHr: maxPrice, MinReliability: 0.90, DiskGB: 40},
		Model: core.ModelSpec{
			Ref: model, Source: core.SourceHuggingFace,
			ServedName: "e2e", Quantization: "fp16", ContextLen: 4096,
		},
		DiskGB:    40,
		HFToken:   secret.New(os.Getenv("HF_TOKEN")),
		LocalPort: 0, // kernel-chosen, so a stray local 8000 cannot fail the run
	})
	if err != nil {
		t.Fatalf("bring-up: %v", err)
	}
	defer live.Close()

	rig := live.Rig
	t.Logf("READY  rig=%s instance=%s %s $%.3f/hr  endpoint=%s",
		rig.ID, rig.Instance.InstanceID, rig.Offer.GPUModel, rig.Offer.PriceHr, live.Endpoint)

	// The thing the operator actually does: a completion through the stable
	// local endpoint, authenticated with the client token.
	t.Run("completion", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"model":      "e2e",
			"messages":   []map[string]string{{"role": "user", "content": "Reply with the single word: pong"}},
			"max_tokens": 16,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			live.Endpoint+"/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+live.ClientToken.Reveal())

		resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
		if err != nil {
			t.Fatalf("completion through the tunnel: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
			Usage struct{ TotalTokens int } `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if len(out.Choices) == 0 {
			t.Fatal("no completion came back")
		}
		t.Logf("model said: %q (%d tokens)",
			strings.TrimSpace(out.Choices[0].Message.Content), out.Usage.TotalTokens)
	})

	// FR-SEC-09: the local endpoint requires a credential.
	t.Run("unauthenticated is refused", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			live.Endpoint+"/chat/completions", strings.NewReader(`{}`))
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})

	// The teardown under test, rather than the cleanup safety net.
	t.Run("down confirms absence", func(t *testing.T) {
		live.Close()
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer dcancel()
		if err := o.Down(dctx, rig, nil); err != nil {
			t.Fatalf("down: %v", err)
		}
		if rig.State != core.StateDestroyed {
			t.Errorf("state = %s, want DESTROYED", rig.State)
		}
		c := rig.End.Cost
		t.Logf("ran %s, total $%.4f (compute $%.4f, storage $%.4f, boot $%.4f)",
			c.Ran.Round(time.Second), c.TotalUSD, c.ComputeUSD, c.StorageUSD, c.BootUSD)
		if c.TotalUSD <= 0 {
			t.Error("a rig that served should have accrued cost")
		}
	})
}

// sweepOrphans destroys anything still carrying a LARRI label, by label rather
// than by what the happy path recorded — because the failure that matters is
// the one where the happy path did not record it.
func sweepOrphans(t *testing.T, p *vastai.Provider, st *state.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	live, err := p.List(ctx)
	if err != nil {
		t.Errorf("SWEEP FAILED to list: %v — CHECK THE VAST.AI DASHBOARD", err)
		return
	}
	known := map[string]bool{}
	if rigs, err := st.List(); err == nil {
		for _, r := range rigs {
			known[r.ID] = true
		}
	}
	var stragglers []string
	for _, inst := range live {
		id, ours := inst.RigID()
		if !ours || !known[id] {
			continue
		}
		t.Logf("sweep: destroying %s (rig %s, running=%v)", inst.InstanceID, id, inst.Running)
		if err := p.Destroy(ctx, inst.InstanceID); err != nil {
			t.Errorf("sweep: destroy %s: %v", inst.InstanceID, err)
		}
		stragglers = append(stragglers, inst.InstanceID)
	}
	if len(stragglers) == 0 {
		return
	}
	for attempt := 0; attempt < 12; attempt++ {
		time.Sleep(5 * time.Second)
		live, err = p.List(ctx)
		if err != nil {
			continue
		}
		remaining := 0
		for _, inst := range live {
			if id, ours := inst.RigID(); ours && known[id] {
				remaining++
			}
		}
		if remaining == 0 {
			t.Log("sweep: confirmed absent")
			return
		}
	}
	t.Errorf("SWEEP UNCONFIRMED for %s — CHECK THE VAST.AI DASHBOARD",
		fmt.Sprint(stragglers))
}
