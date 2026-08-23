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
	keys, err := sshx.NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	// A tiny image: this probe is about the container's powers, not about
	// serving anything.
	inst, err := p.Create(ctx, sel.Selected.Offer, provider.CreateSpec{
		Image: "alpine:latest", DiskGB: 16, Label: "larri-probe",
		OnStart: keys.OnStartScript(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("rented %s (%s $%.3f/hr)", inst.InstanceID, sel.Selected.Offer.GPUModel,
		sel.Selected.Offer.PriceHr)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer dcancel()
		if err := p.Destroy(dctx, inst.InstanceID); err != nil {
			t.Errorf("DESTROY FAILED for %s: %v — CHECK THE DASHBOARD", inst.InstanceID, err)
		}
		t.Logf("destroyed %s", inst.InstanceID)
	}()

	o := &Orchestrator{Provider: p, BootPollInterval: 15 * time.Second,
		BootCap: 12 * time.Minute, BootStallTimeout: 6 * time.Minute}
	rig := &core.Rig{Instance: inst}
	live, err := o.waitForSSH(ctx, rig)
	if err != nil {
		t.Fatalf("ssh never came up: %v", err)
	}
	_, client, err := o.pinAndDial(ctx, live, keys)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess := client.Session()

	// What is this container, and what is it allowed to do?
	probe := `echo "pid1=$(ps -p 1 -o comm= 2>/dev/null || cat /proc/1/comm)"; ` +
		`echo "halt=$(command -v halt || echo missing)"; ` +
		`echo "poweroff=$(command -v poweroff || echo missing)"; ` +
		`echo "shutdown=$(command -v shutdown || echo missing)"; ` +
		`echo "capbnd=$(grep CapBnd /proc/self/status | awk '{print $2}')"; ` +
		`echo "incontainer=$(test -f /.dockerenv && echo yes || echo no)"`
	out, err := sess.Run(ctx, probe)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("host powers:\n%s", strings.TrimSpace(string(out)))

	// Does halting actually work? Try each in turn and report what happened.
	for _, attempt := range []struct{ name, cmd string }{
		{"halt -f", "halt -f 2>&1 || echo 'refused: '$?"},
		{"poweroff -f", "poweroff -f 2>&1 || echo 'refused: '$?"},
		{"kill -TERM 1", "kill -TERM 1 2>&1 || echo 'refused: '$?"},
		{"kill -KILL 1", "kill -KILL 1 2>&1 || echo 'refused: '$?"},
	} {
		r, _ := sess.Run(ctx, attempt.cmd)
		t.Logf("  %-14s → %s", attempt.name, strings.TrimSpace(string(r)))

		// Give the provider a moment, then ask whether it noticed.
		time.Sleep(30 * time.Second)
		cur, err := p.Get(ctx, inst.InstanceID)
		if err != nil || cur == nil {
			t.Logf("  %-14s → provider: gone/unreadable (%v)", attempt.name, err)
			t.Log("VERDICT: the container can stop itself")
			return
		}
		t.Logf("  %-14s → provider: running=%v status=%q", attempt.name, cur.Running, cur.Status)
		if !cur.Running {
			t.Log("VERDICT: the container can stop itself, and the provider notices")
			return
		}
	}
	t.Log("VERDICT: a vast container CANNOT stop its own billing — " +
		"the host-side halt is not a viable backstop on this provider")
}
