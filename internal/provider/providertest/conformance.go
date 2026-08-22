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
}

// Run executes the conformance suite.
func Run(t *testing.T, h Harness) {
	t.Run("NameIsStable", func(t *testing.T) {
		if h.Provider.Name() == "" {
			t.Error("a provider must identify itself; state records it")
		}
		if h.Provider.Name() != h.Provider.Name() {
			t.Error("Name must be stable")
		}
	})

	t.Run("AbsentInstanceIsNilNotError", func(t *testing.T) {
		inst, err := h.Provider.Get(context.Background(), "definitely-not-an-instance")
		if err != nil {
			t.Fatalf("absence must not be an error: %v", err)
		}
		if inst != nil {
			t.Fatal("absent means nil, which is the only proof of destruction")
		}
	})

	t.Run("DestroyIsIdempotent", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if err := h.Provider.Destroy(context.Background(), "already-gone"); err != nil {
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
