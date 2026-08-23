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
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/config"
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

	// The marker is sealed when a key is configured, so the run exercises the
	// path an operator actually gets rather than a default nothing sets.
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
		Store: st, Provider: p, Runtime: vllm.New(),
		Resolver: sizing.NewHFResolver(secret.New(os.Getenv("HF_TOKEN"))),
		Policy:   rank.DefaultPolicy(),
		Deadline: 20 * time.Minute,
		// Live runs put the host failure rate on the cheap tier well above
		// what three attempts covers: dead containers behind live contracts,
		// hosts that never boot, image pulls that never start. Reliability
		// scores did not predict any of it — every failed machine scored 0.98
		// or better. So the answer is more attempts, each cheap because the
		// endpoint probe now ends a bad one in minutes rather than tens.
		MaxHostAttempts: 6,
		Events:          events,
		LabelSealer:     sealer,
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
	// Indirect, because the resume subtest replaces live with the adopted
	// one and a direct defer would close the old pointer and leak the new.
	defer func() { live.Close() }()

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
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
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

	// FR-SUP-13..16: the rig outlives the process that made it.
	//
	// This is the subtest that needs real hardware. Everything it exercises —
	// the provider installing a key on a running instance, the credential
	// still being in the server's argv, the host key matching what was pinned
	// — is a claim about someone else's system that a fake would only restate.
	t.Run("resume rebuilds the tunnel after the process dies", func(t *testing.T) {
		port := rig.LocalPort
		before := processIdentity(ctx, t, live)
		t.Logf("before: %s", before)

		// Everything a process death takes with it: the SSH connection, the
		// forward, the proxy, the ephemeral key, the client token.
		live.Close()

		adopted, err := o.Adopt(ctx, rig.ID)
		if adopted != nil {
			t.Cleanup(func() { adopted.Close() })
		}
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		live = adopted

		// AC-6.1: the same local port, or clients would need reconfiguring
		// and the recovery would be worthless to the tools that matter.
		if adopted.Rig.LocalPort != port {
			t.Errorf("local port %d after resume, was %d", adopted.Rig.LocalPort, port)
		}

		// AC-6.2: re-attached, not relaunched. A restarted server would show
		// a new pid and would have paid the model-load cost a second time.
		after := processIdentity(ctx, t, adopted)
		t.Logf("after:  %s", after)
		if before != "" && after != before {
			t.Errorf("the runtime restarted:\n  before %s\n  after  %s", before, after)
		}

		// AC-6.3: no private key was written anywhere to make this work.
		assertNoPrivateKeyOnDisk(t, st.Dir())

		// And the only proof that counts.
		body, _ := json.Marshal(map[string]any{
			"model":      "e2e",
			"messages":   []map[string]string{{"role": "user", "content": "Reply with the single word: back"}},
			"max_tokens": 16,
		})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			adopted.Endpoint+"/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adopted.ClientToken.Reveal())
		resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
		if err != nil {
			t.Fatalf("completion after resume: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if len(out.Choices) == 0 {
			t.Fatal("no completion came back through the rebuilt tunnel")
		}
		t.Logf("model said after resume: %q", strings.TrimSpace(out.Choices[0].Message.Content))
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

// processIdentity returns the runtime's pid and start time, which together
// change if and only if the server was restarted.
func processIdentity(ctx context.Context, t *testing.T, l *Live) string {
	t.Helper()
	const cmd = `pid=$(pgrep -f '[v]llm serve' | head -1); ` +
		`[ -z "$pid" ] && pid=$(pgrep -f '[v]llm\.entrypoints\.openai' | head -1); ` +
		`[ -z "$pid" ] && { echo none; exit 0; }; ` +
		`echo "pid=$pid started=$(ps -o lstart= -p $pid)"`
	out, err := l.ssh.Session().Run(ctx, cmd)
	if err != nil {
		t.Logf("could not read the runtime process: %v", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// assertNoPrivateKeyOnDisk enforces FR-STATE-05 by inspection rather than by
// intent. Recovery is built so that no key ever needs storing; this is what
// would catch a future change that quietly stored one to make something work.
func assertNoPrivateKeyOnDisk(t *testing.T, dir string) {
	t.Helper()
	markers := []string{"PRIVATE KEY", "BEGIN OPENSSH"}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range markers {
			if strings.Contains(string(b), m) {
				t.Errorf("private key material in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Errorf("walk state dir: %v", err)
	}
}
