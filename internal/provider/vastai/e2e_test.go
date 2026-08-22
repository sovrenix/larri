//go:build e2e

// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Live verification against the real Vast.ai API.
//
// Build-tagged and env-gated so it can never run as part of `go test ./...`
// (NFR-09). Run deliberately:
//
//	VASTAI_API_KEY=... go test -tags e2e -v ./internal/provider/vastai/
//
// By default this file spends NOTHING: it exercises search and list only,
// which are read operations. The one test that creates an instance is gated
// behind a second, separate variable and tears down in a t.Cleanup that also
// runs on panic.
package vastai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/state"
)

func liveProvider(t *testing.T) *Provider {
	t.Helper()
	key := os.Getenv("VASTAI_API_KEY")
	if key == "" {
		t.Skip("VASTAI_API_KEY not set")
	}
	p := New(secret.New(key))
	p.OnDrift = func(err error) { t.Errorf("SHAPE DRIFT: %v", err) }
	p.OnNotice = func(m string) { t.Logf("notice: %s", m) }
	return p
}

// The contract test that matters: the live API still returns the fields this
// adapter depends on, in the units it expects. Spends nothing.
func TestLiveSearchShapeStillMatches(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	offers, err := p.Search(ctx, core.Criteria{VRAMPerGPUGB: 24, MaxPriceHr: 5.0})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(offers) == 0 {
		t.Skip("no offers matched; cannot verify shape")
	}
	t.Logf("normalised %d offers", len(offers))

	for i, o := range offers {
		if i < 5 {
			t.Logf("  %-18s %dx%dGB  $%.3f/hr  rel=%.2f  cuda=%s  %s",
				o.GPUModel, o.GPUCount, o.VRAMPerGPUGB, o.PriceHr,
				o.Reliability, o.CUDAVersion, o.Region)
		}
		// The unit assumption, checked against reality rather than asserted.
		if o.VRAMPerGPUGB < 4 || o.VRAMPerGPUGB > 200 {
			t.Errorf("offer %s: VRAM %d GB is implausible — gpu_ram unit may have changed",
				o.OfferID, o.VRAMPerGPUGB)
		}
		if o.PriceHr <= 0 || o.PriceHr > 100 {
			t.Errorf("offer %s: $%.4f/hr is implausible", o.OfferID, o.PriceHr)
		}
		if o.Reliability < 0 || o.Reliability > 1 {
			t.Errorf("offer %s: reliability %.3f outside 0..1", o.OfferID, o.Reliability)
		}
		if o.GPUModel == "" {
			t.Errorf("offer %s: no GPU model", o.OfferID)
		}
	}
}

// Verifies pagination against the live endpoint and, critically, that nothing
// unaccounted-for is running on the account.
func TestLiveListAndOrphanCheck(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	all, err := p.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	t.Logf("account holds %d instances (running and stopped)", len(all))
	for _, i := range all {
		rig, ours := i.RigID()
		marker := "not ours"
		if ours {
			marker = "larri rig " + rig
		}
		t.Logf("  %-12s running=%-5v $%.3f/hr storage=$%.4f/hr  %s",
			i.InstanceID, i.Running, i.PriceHr, i.StorageHr, marker)
		if ours && !state.ValidID(rig) {
			t.Errorf("instance %s carries a malformed rig label %q", i.InstanceID, rig)
		}
	}
	if len(all) > 0 {
		t.Logf("NOTE: instances above are billing. Stopped ones still bill storage.")
	}
}

// The only test that spends money. Doubly gated, minimal, and torn down in a
// Cleanup that runs on panic as well as on failure.
func TestLiveCreateAndDestroy(t *testing.T) {
	if os.Getenv("LARRI_E2E_SPEND") != "yes" {
		t.Skip("set LARRI_E2E_SPEND=yes to run the test that rents hardware")
	}
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	offers, err := p.Search(ctx, core.Criteria{VRAMPerGPUGB: 8, MaxPriceHr: 0.30})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) == 0 {
		t.Skip("no cheap offer available to test with")
	}
	cheapest := offers[0]
	for _, o := range offers {
		if o.PriceHr < cheapest.PriceHr {
			cheapest = o
		}
	}
	rigID, err := state.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("renting %s $%.3f/hr as rig %s", cheapest.GPUModel, cheapest.PriceHr, rigID)

	inst, err := p.Create(ctx, cheapest, provider.CreateSpec{
		Image:  "vastai/base-image:latest",
		DiskGB: 10,
		Label:  core.LabelKey + ":" + rigID,
	})

	// Registered before the error is examined: a create that returned an
	// error may still have produced an instance (R-07).
	t.Cleanup(func() {
		tctx, tcancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer tcancel()
		live, lerr := p.List(tctx)
		if lerr != nil {
			t.Errorf("TEARDOWN: could not list: %v — CHECK THE DASHBOARD", lerr)
			return
		}
		for _, i := range live {
			if id, ours := i.RigID(); ours && id == rigID {
				t.Logf("teardown: destroying %s", i.InstanceID)
				if derr := p.Destroy(tctx, i.InstanceID); derr != nil {
					t.Errorf("TEARDOWN FAILED for %s: %v — CHECK THE DASHBOARD", i.InstanceID, derr)
				}
			}
		}
		// Absence is the proof, not the delete call's return value.
		for attempt := 0; attempt < 10; attempt++ {
			time.Sleep(3 * time.Second)
			live, lerr = p.List(tctx)
			if lerr != nil {
				continue
			}
			found := false
			for _, i := range live {
				if id, ours := i.RigID(); ours && id == rigID {
					found = true
				}
			}
			if !found {
				t.Log("teardown: confirmed absent")
				return
			}
		}
		t.Errorf("TEARDOWN UNCONFIRMED for rig %s — CHECK THE DASHBOARD", rigID)
	})

	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inst == nil || inst.InstanceID == "" {
		t.Fatal("create returned no instance id")
	}
	t.Logf("created instance %s", inst.InstanceID)

	got, err := p.Get(ctx, inst.InstanceID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("the instance we just created must be findable by Get")
	}
	if id, ours := got.RigID(); !ours || id != rigID {
		t.Errorf("label round-trip failed: got %q ours=%v, want %s", id, ours, rigID)
	}
	if got.SSHHost == "" && got.Running {
		t.Log("note: ssh_host empty while running — may populate later in boot")
	}
	if !strings.EqualFold(got.Provider, "vastai") {
		t.Errorf("provider = %q", got.Provider)
	}
}

// Discovers the server's real ceiling on search results.
//
// A live run returned exactly 500 offers, which is the value LARRI asks for —
// so it is unknown whether 500 is our cap or Vast's. That matters: ranking
// weights fit at 0.20 to avoid over-paying for unusable VRAM, and it can only
// weigh candidates it was given. Spends nothing.
func TestLiveSearchCeiling(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, limit := range []int{100, 500, 1000, 5000} {
		req := searchRequest{
			Limit:    limit,
			Type:     "on-demand",
			Order:    [][]string{{"dph_total", "asc"}},
			Rentable: &boolFilter{Eq: true},
		}
		var resp searchResponse
		if err := p.c.do(ctx, "POST", pathSearch, req, &resp); err != nil {
			t.Logf("limit %-5d -> error: %v", limit, err)
			continue
		}
		got := len(resp.Offers)
		note := ""
		switch {
		case got < limit:
			note = "  <- full result set; this is every matching offer"
		case got == limit:
			note = "  <- capped at the requested limit"
		}
		t.Logf("limit %-5d -> %4d offers%s", limit, got, note)
	}
	t.Log("if the count stops rising, that plateau is the server ceiling and " +
		"Search must report truncation above it")
}

// Confirms the pricing distribution FR-SRCH-08 is about.
//
// A live run showed a 32 GB V100 at $0.029/hr, roughly an order of magnitude
// under market. Ranking weights price at 0.40, so scoring steers straight at
// offers like these — which is also exactly where a host fishing for renters
// would price. Spends nothing; prints the distribution rather than asserting,
// since "anomalous" is a property of the market on the day.
func TestLivePriceDistribution(t *testing.T) {
	p := liveProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	offers, err := p.Search(ctx, core.Criteria{GPUModel: []string{"RTX 4090"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(offers) < 5 {
		t.Skipf("only %d offers; not enough to characterise", len(offers))
	}
	var sum float64
	lo, hi := offers[0].PriceHr, offers[0].PriceHr
	for _, o := range offers {
		sum += o.PriceHr
		if o.PriceHr < lo {
			lo = o.PriceHr
		}
		if o.PriceHr > hi {
			hi = o.PriceHr
		}
	}
	mean := sum / float64(len(offers))
	t.Logf("RTX 4090: n=%d  min=$%.3f  mean=$%.3f  max=$%.3f", len(offers), lo, mean, hi)
	if lo < mean/4 {
		t.Logf("cheapest is %.1fx below mean: the shape FR-SRCH-08 must flag "+
			"rather than rank first", mean/lo)
	}
}
