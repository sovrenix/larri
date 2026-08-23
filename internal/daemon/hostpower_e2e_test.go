//go:build e2e

// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// What powers does a rented container actually have over itself?
//
// The dead-man switch rests on one assumption — that a host can stop itself —
// and a live run said otherwise: the instance kept reporting `running` long
// after the watchdog should have halted it. This probe settles which half is
// wrong, the script or the assumption, because guessing would mean shipping a
// backstop that backs nothing up.
//
//	VASTAI_API_KEY=... LARRI_E2E_SPEND=yes \
//	  go test -tags e2e -v -timeout 25m -run TestHostSelfStopPowers ./internal/daemon/
package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/provider/vastai"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sshx"
)

func TestHostSelfStopPowers(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to rent real hardware")
	}
	key := os.Getenv("VASTAI_API_KEY")
	if key == "" {
		t.Skip("VASTAI_API_KEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	p := vastai.New(secret.New(key))
	offers, err := p.Search(ctx, core.Criteria{MaxPriceHr: 0.20, MinReliability: 0.95, DiskGB: 16})
	if err != nil {
		t.Fatal(err)
	}
	sel := rank.Select(offers, core.Criteria{MaxPriceHr: 0.20, MinReliability: 0.95},
		func(core.Offer) (bool, string) { return true, "" }, rank.DefaultPolicy())
	if sel.Selected == nil {
		t.Fatal("no offer")
	}
	// A real tag, not `latest`: vastai/base-image publishes only versioned
	// tags, and asking for one that does not exist produces a container that
	// never starts — which looks exactly like a dead host at the ssh probe
	// and cost five rentals to tell apart.
	image := os.Getenv("LARRI_PROBE_IMAGE")
	if image == "" {
		image = "vastai/base-image:cuda-12.9.2-auto"
	}

	// Fall back across hosts, because roughly half of the cheap tier never
	// answers (§12.2.2) and a probe without fallback mostly measures that
	// instead of what it came to measure.
	var (
		inst   *core.Instance
		client *sshx.Client
		keys   *sshx.KeyPair
	)
	o := &Orchestrator{Provider: p, BootPollInterval: 15 * time.Second,
		BootCap: 10 * time.Minute, BootStallTimeout: 5 * time.Minute,
		EndpointStallLimit: 3 * time.Minute}

	for attempt, cand := range eligible(sel, 5) {
		if attempt > 0 {
			t.Logf("--- attempt %d ---", attempt+1)
		}
		keys, err = sshx.NewKeyPair()
		if err != nil {
			t.Fatal(err)
		}
		got, cerr := p.Create(ctx, cand.Offer, provider.CreateSpec{
			Image: image, DiskGB: 16, Label: "larri-probe",
			OnStart: keys.OnStartScript(),
		})
		if cerr != nil {
			t.Logf("create: %v", cerr)
			continue
		}
		t.Logf("rented %s (%s $%.3f/hr)", got.InstanceID, cand.Offer.GPUModel, cand.Offer.PriceHr)

		live, werr := o.waitForSSH(ctx, &core.Rig{Instance: got})
		if werr == nil {
			_, c, derr := o.pinAndDial(ctx, live, keys)
			if derr == nil {
				inst, client = got, c
				break
			}
			werr = derr
		}
		t.Logf("unusable: %v", shortErr(werr))
		dctx, dcancel := context.WithTimeout(context.Background(), 4*time.Minute)
		_ = p.Destroy(dctx, got.InstanceID)
		dcancel()
	}
	if inst == nil {
		t.Fatal("no host came up")
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer dcancel()
		if err := p.Destroy(dctx, inst.InstanceID); err != nil {
			t.Errorf("DESTROY FAILED for %s: %v — CHECK THE DASHBOARD", inst.InstanceID, err)
		}
		t.Logf("destroyed %s", inst.InstanceID)
	}()
	defer client.Close()
	sess := client.Session()

	// Everything worth knowing, gathered before anything is broken — the
	// stop attempts below may take sshd with them.
	probe := `echo "pid1=$(cat /proc/1/comm 2>/dev/null)"; ` +
		`echo "pid1cmd=$(tr '\0' ' ' < /proc/1/cmdline 2>/dev/null | cut -c1-60)"; ` +
		`echo "halt=$(command -v halt || echo missing)"; ` +
		`echo "poweroff=$(command -v poweroff || echo missing)"; ` +
		`echo "shutdown=$(command -v shutdown || echo missing)"; ` +
		`echo "capbnd=$(grep CapBnd /proc/self/status | awk '{print $2}')"; ` +
		`echo "dockerenv=$(test -f /.dockerenv && echo yes || echo no)"; ` +
		`echo "pidns=$(readlink /proc/self/ns/pid)"`
	out, err := sess.Run(ctx, probe)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("what this container is:\n%s", strings.TrimSpace(string(out)))

	// Each stop method in turn. The provider's view is the verdict and needs
	// no ssh, which matters because a successful attempt ends the session
	// that issued it.
	for _, attempt := range []struct{ name, cmd string }{
		{"halt -f", "halt -f 2>&1; echo rc=$?"},
		{"poweroff -f", "poweroff -f 2>&1; echo rc=$?"},
		{"kill -TERM 1", "kill -TERM 1 2>&1; echo rc=$?"},
		{"kill -KILL 1", "kill -KILL 1 2>&1; echo rc=$?"},
	} {
		r, rerr := sess.Run(ctx, attempt.cmd)
		msg := strings.TrimSpace(string(r))
		if rerr != nil {
			msg = "session died: " + shortErr(rerr)
		}
		t.Logf("  %-14s → %s", attempt.name, msg)

		// Give the provider time to notice, checking as it goes.
		stopped := false
		for i := 0; i < 6; i++ {
			time.Sleep(20 * time.Second)
			cur, gerr := p.Get(ctx, inst.InstanceID)
			if gerr != nil {
				continue
			}
			if cur == nil {
				t.Logf("  %-14s → provider: instance gone", attempt.name)
				t.Log("VERDICT: the container CAN stop itself, and the bill ends with it")
				return
			}
			if !cur.Running {
				t.Logf("  %-14s → provider: running=false status=%q after %ds",
					attempt.name, cur.Status, (i+1)*20)
				t.Log("VERDICT: the container CAN stop itself; compute billing ends")
				stopped = true
				break
			}
		}
		if stopped {
			return
		}
		cur, _ := p.Get(ctx, inst.InstanceID)
		if cur != nil {
			t.Logf("  %-14s → provider: still running=%v status=%q after 2m",
				attempt.name, cur.Running, cur.Status)
		}
	}
	t.Log("VERDICT: a vast container CANNOT stop its own billing — " +
		"the host-side halt is not a viable backstop on this provider")
}

// eligible returns the cheapest usable offers, so the probe can fall back.
func eligible(sel rank.Result, n int) []rank.Candidate {
	out := make([]rank.Candidate, 0, n)
	for _, c := range sel.Candidates {
		if c.Eligible() && len(out) < n {
			out = append(out, c)
		}
	}
	return out
}
