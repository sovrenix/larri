// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package providertest is the conformance suite every Provider must pass.
//
// It exists because adapters drift. The fake and the Vast.ai adapter disagreed
// on how the ownership label is normalised — one stored `larri:<id>` where the
// other stored `<id>` — which meant every test written against the fake was
// verifying behaviour the real adapter did not have. That divergence is
// invisible until a live run, and by then it is orphan detection that is
// broken.
//
// So the contract is asserted once, here, and both adapters run it.
package providertest

import (
	"context"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider"
)

// Harness supplies a provider and the means to drive it.
type Harness struct {
	// Provider under test.
	Provider provider.Provider

	// AnOffer returns an offer this provider will accept in Create.
	AnOffer func(t *testing.T) core.Offer

	// SkipCreate marks adapters that cannot create without spending money.
	// The read-only half of the suite still runs.
	SkipCreate bool

	// Criteria the provider's Search should satisfy. Zero means unfiltered.
	Criteria core.Criteria

	// AbsentID is a well-formed id that does not exist.
	//
	// Well-formed matters, and running this live is what showed why: Vast
	// answers a *malformed* id with 400 and a well-formed absent one with
	// 404. Testing with "definitely-not-an-instance" therefore asserted that
	// a parse failure looks like absence, which is not the contract and not
	// something an adapter should pretend. Absent and unintelligible are
	// different conditions and only the first has to be nil.
	AbsentID string

	// Stop puts an instance into the state where it exists, is not running,
	// and still bills for storage. Nil for adapters that cannot reach it
	// without spending — the check is then skipped rather than faked, since
	// a fake pass here is worse than no pass at all.
	Stop func(t *testing.T, instanceID string)
}

// Run executes the conformance suite.
func Run(t *testing.T, h Harness) {
	runSearchContract(t, h)
	runStoppedContract(t, h)

	t.Run("NameIsStable", func(t *testing.T) {
		if h.Provider.Name() == "" {
			t.Error("a provider must identify itself; state records it")
		}
		if h.Provider.Name() != h.Provider.Name() {
			t.Error("Name must be stable")
		}
	})

	t.Run("AbsentInstanceIsNilNotError", func(t *testing.T) {
		inst, err := h.Provider.Get(context.Background(), h.absentID())
		if err != nil {
			t.Fatalf("absence must not be an error: %v", err)
		}
		if inst != nil {
			t.Fatal("absent means nil, which is the only proof of destruction")
		}
	})

	t.Run("DestroyIsIdempotent", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if err := h.Provider.Destroy(context.Background(), h.absentID()); err != nil {
				t.Fatalf("destroying an absent instance must succeed (attempt %d): %v", i, err)
			}
		}
	})

	if h.SkipCreate {
		return
	}

	t.Run("LabelNormalisesToTheBareRigID", func(t *testing.T) {
		ctx := context.Background()
		const rigID = "01J9ZTESTRIGIDTESTRIGIDXX"
		inst, err := h.Provider.Create(ctx, h.AnOffer(t), provider.CreateSpec{
			Label: core.LabelKey + ":" + rigID,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = h.Provider.Destroy(ctx, inst.InstanceID) })

		// The whole point of the label is that reconciliation can compare it
		// to a rig ID it already holds. An adapter that stores the prefixed
		// form makes every comparison fail, and the orphan stays invisible.
		got, ours := inst.RigID()
		if !ours {
			t.Fatal("a created instance must be attributable to LARRI")
		}
		if got != rigID {
			t.Fatalf("RigID() = %q, want the bare id %q — the prefix must be stripped "+
				"at the adapter boundary, not carried upward", got, rigID)
		}
		// And the same must hold when read back, not only when returned.
		all, err := h.Provider.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, i := range all {
			if i.InstanceID == inst.InstanceID {
				found = true
				if id, ok := i.RigID(); !ok || id != rigID {
					t.Errorf("List gave RigID() = %q ours=%v, want %q", id, ok, rigID)
				}
			}
		}
		if !found {
			t.Error("a created instance must appear in List, or orphan detection cannot see it")
		}
	})

	t.Run("CreatedInstanceIsRetrievable", func(t *testing.T) {
		ctx := context.Background()
		inst, err := h.Provider.Create(ctx, h.AnOffer(t), provider.CreateSpec{
			Label: core.LabelKey + ":01J9ZRETRIEVABLETESTXXXXXX",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer h.Provider.Destroy(ctx, inst.InstanceID)

		got, err := h.Provider.Get(ctx, inst.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("an instance that was just created must be retrievable")
		}
		if got.Provider != h.Provider.Name() {
			t.Errorf("Provider = %q, want %q", got.Provider, h.Provider.Name())
		}
	})

	t.Run("DestroyThenAbsent", func(t *testing.T) {
		ctx := context.Background()
		inst, err := h.Provider.Create(ctx, h.AnOffer(t), provider.CreateSpec{
			Label: core.LabelKey + ":01J9ZDESTROYTESTXXXXXXXXXX",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := h.Provider.Destroy(ctx, inst.InstanceID); err != nil {
			t.Fatal(err)
		}
		got, err := h.Provider.Get(ctx, inst.InstanceID)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Fatal("after a successful destroy the instance must be absent, " +
				"because absence is what teardown checks")
		}
	})
}

// runSearchContract asserts what the ranker needs from every Search result.
//
// Selection is downstream of normalisation, and a field an adapter forgets to
// fill does not fail loudly — it produces an offer that is silently rejected,
// or worse, silently chosen. The reliability floor was exactly this: an
// adapter that reports no score would have had its entire catalogue excluded
// with "reliability 0.00 below floor 0.90", which reads as an empty market
// rather than a modelling mistake.
func runSearchContract(t *testing.T, h Harness) {
	t.Run("SearchFillsWhatSelectionReads", func(t *testing.T) {
		ctx := context.Background()
		offers, err := h.Provider.Search(ctx, h.Criteria)
		if err != nil {
			t.Skipf("search unavailable: %v", err)
		}
		if len(offers) == 0 {
			t.Skip("no offers to check")
		}
		name := h.Provider.Name()
		seen := map[string]bool{}
		for i, o := range offers {
			if i >= 200 {
				break
			}
			switch {
			case o.Provider != name:
				t.Errorf("offer %d says provider %q, adapter is %q — orphan attribution "+
					"and cost records both key on this", i, o.Provider, name)
			case o.OfferID == "":
				t.Error("an offer with no id cannot be purchased or reported")
			case o.GPUModel == "":
				t.Error("an offer with no GPU model cannot be ranked: the price-outlier " +
					"floor medians by hardware class")
			case o.PriceHr <= 0:
				t.Errorf("offer %s priced at %v; cost accounting and every ceiling "+
					"depend on this", o.OfferID, o.PriceHr)
			case o.VRAMTotalGB() <= 0:
				t.Errorf("offer %s reports no VRAM; the fit test cannot run", o.OfferID)
			}
			if seen[o.OfferID] {
				t.Errorf("offer id %s appears twice; selection would rank one listing "+
					"against itself", o.OfferID)
			}
			seen[o.OfferID] = true

			// Reliability is optional, but a value outside 0..1 is a
			// normalisation bug rather than a missing feature.
			if o.Reliability < 0 || o.Reliability > 1 {
				t.Errorf("offer %s reliability %v is outside 0..1", o.OfferID, o.Reliability)
			}
		}
	})

}

// runStoppedContract asserts the property STOPPED detection rests on.
//
// A List that omitted non-running instances would report a storage-billing
// resource as absent, and teardown would journal it DESTROYED while it billed
// on (R-13). It is the single most expensive thing an adapter can get wrong,
// and it cannot be caught by reading the code.
func runStoppedContract(t *testing.T, h Harness) {
	if h.SkipCreate || h.Stop == nil {
		return
	}
	t.Run("StoppedInstancesStayVisible", func(t *testing.T) {
		ctx := context.Background()
		inst, err := h.Provider.Create(ctx, h.AnOffer(t), provider.CreateSpec{
			Image: "test", DiskGB: 10, Label: core.LabelKey + ":01STOPPED",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		defer h.Provider.Destroy(ctx, inst.InstanceID)

		h.Stop(t, inst.InstanceID)

		got, err := h.Provider.Get(ctx, inst.InstanceID)
		if err != nil {
			t.Fatalf("get after stop: %v", err)
		}
		if got == nil {
			t.Fatal("a stopped instance read as absent; it still bills for storage " +
				"and teardown would record it destroyed")
		}
		if got.Running {
			t.Error("a stopped instance reports Running")
		}
		all, err := h.Provider.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, i := range all {
			if i.InstanceID == inst.InstanceID {
				return
			}
		}
		t.Error("a stopped instance is missing from List; the reconciler would " +
			"never see it and it would bill unnoticed")
	})
}

// absentID returns a well-formed identifier that does not exist.
func (h Harness) absentID() string {
	if h.AbsentID != "" {
		return h.AbsentID
	}
	return "999999999"
}
