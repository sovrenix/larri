// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// Summary is what every surface shows about one rig.
//
// One definition, because the CLI and the MCP tool each built their own and
// drifted: the CLI showed an ID, a state and a cost, while the tool showed the
// model and a price but dropped the provider whenever there was no instance.
// An operator asking "what is this rig" got a different answer depending on
// which front-end they asked (invariant 6).
type Summary struct {
	ID    string
	State core.LifecycleState

	Provider string
	Hardware string // "2× A100 SXM 160GB": the card count is part of the identity
	GPUModel string
	GPUCount int
	VRAMGB   int    // summed across the cards
	Region   string // empty when the provider does not say

	// Instance is the provider's name for the machine. Empty means LARRI
	// holds no record of one, which is not the same as there being none: a
	// create whose answer was lost leaves exactly this.
	Instance string

	// PriceHr is what the provider says the machine costs, once it has said
	// so, and the rate quoted at selection until then. QuotedHr is always the
	// quote. They are both carried because they disagree: a RunPod pod quoted
	// at $2.78/hr from the catalogue billed at $3.18/hr, and only a surface
	// that shows both lets an operator see that without a provider dashboard.
	PriceHr  float64
	QuotedHr float64

	Model        string
	Quantization string
	Served       string
	Runtime      core.RuntimeKind

	// Endpoint is the local /v1 address, and only while the rig can answer
	// on it. A destroyed rig's port belongs to whatever binds it next.
	Endpoint string

	CreatedAt time.Time
	Cost      core.CostSummary
	End       *core.Termination
}

// Summarise renders a rig and its journal for display.
func Summarise(r *core.Rig, entries []Entry, now time.Time) Summary {
	s := Summary{
		ID: r.ID, State: r.State,
		Provider: r.Offer.Provider, Hardware: r.Offer.Hardware(),
		GPUModel: r.Offer.GPUModel, GPUCount: r.Offer.GPUCount,
		VRAMGB: r.Offer.VRAMTotalGB(), Region: r.Offer.Region,
		PriceHr: r.BilledPriceHr(), QuotedHr: r.Offer.PriceHr,
		Model: r.Model.Ref, Quantization: r.Model.Quantization,
		Served: r.Model.ServedName, Runtime: r.Runtime,
		CreatedAt: r.CreatedAt, End: r.End,
		Cost: CostFor(entries, r.ID, now),
	}
	if r.Instance != nil {
		s.Instance = r.Instance.InstanceID
		if s.Provider == "" {
			s.Provider = r.Instance.Provider
		}
	}
	if serving(r.State) && r.LocalPort > 0 {
		s.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1", r.LocalPort)
	}
	return s
}

// PriceDiffers reports whether the provider bills a different rate from the
// one quoted at selection. A cent of rounding is not a difference.
func (s Summary) PriceDiffers() bool {
	d := s.PriceHr - s.QuotedHr
	return s.QuotedHr > 0 && (d > 0.005 || d < -0.005)
}

func serving(st core.LifecycleState) bool {
	return st == core.StateReady || st == core.StateDegraded
}
