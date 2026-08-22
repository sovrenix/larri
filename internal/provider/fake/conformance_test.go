// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package fake_test

import (
	"testing"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/provider/providertest"
)

// The fake runs the same contract as the real adapters, which is what stops
// tests written against it from verifying behaviour the real thing lacks.
func TestFakeConformance(t *testing.T) {
	offer := core.Offer{
		Provider: "fake", OfferID: "a", GPUModel: "A100",
		GPUCount: 1, VRAMPerGPUGB: 80, PriceHr: 1.29, Reliability: 0.98,
	}
	providertest.Run(t, providertest.Harness{
		Provider: fake.New("fake", []core.Offer{offer}, fake.Behaviour{}),
		AnOffer:  func(*testing.T) core.Offer { return offer },
	})
}
