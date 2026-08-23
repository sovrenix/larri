// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/state"
)

// Deps is what the tools need to do their work.
//
// A struct rather than a package-level orchestrator because the driver decides
// the lifetime: an MCP server outlives any one rig, and a tool that captured a
// live orchestrator would be holding a stale one by the second call.
type Deps struct {
	Store *state.Store

	// NewOrchestrator builds one configured for the current environment. It
	// returns an error rather than a zero value so a missing API key is
	// reported to the calling agent, not discovered as a nil dereference.
	NewOrchestrator func(runtimeKind string) (*daemon.Orchestrator, error)

	// Live returns the rig this process is serving, if any. MCP runs beside
	// a session rather than inside it, so this is usually nil.
	Live func() *daemon.Live
}

// Register adds LARRI's operations to a registry.
func Register(r *Registry, d Deps) error {
	for _, t := range larriTools(d) {
		if err := r.Add(t); err != nil {
			return err
		}
	}
	return nil
}

func larriTools(d Deps) []Tool {
	return []Tool{
		{
			Name: "larri_status",
			Description: "List LARRI rigs with state, hourly price, accrued cost, and why past rigs ended. " +
				"Read-only; spends nothing.",
			Schema: Object(map[string]Property{
				"all": {Type: "boolean", Description: "include terminated rigs"},
			}),
			Handler: d.status,
		},
		{
			Name: "larri_plan",
			Description: "Compute the VRAM a model needs and what it would cost to serve, without renting anything. " +
				"Use this before larri_up to check feasibility and price.",
			Schema: Object(map[string]Property{
				"model":        {Type: "string", Description: "model reference, e.g. Qwen/Qwen3-Coder-30B"},
				"quantization": {Type: "string", Description: "fp16, q4_K_M, awq, ..."},
				"context":      {Type: "integer", Description: "context length in tokens"},
			}, "model"),
			Handler: d.plan,
		},
		{
			Name: "larri_search_offers",
			Description: "Search GPU offers that can serve a model and rank them cheapest-first. " +
				"Read-only; spends nothing.",
			Schema: Object(map[string]Property{
				"model":        {Type: "string", Description: "model reference"},
				"quantization": {Type: "string"},
				"context":      {Type: "integer"},
				"gpu":          {Type: "string", Description: "GPU model filter, e.g. 'RTX 4090'"},
				"max_price":    {Type: "number", Description: "ceiling in $/hr"},
				"top":          {Type: "integer", Description: "how many to return (default 10)"},
			}, "model"),
			Handler: d.searchOffers,
		},
		{
			Name:        "larri_logs",
			Description: "Read the runtime log from the rig's host — the account of why a launch failed.",
			Schema: Object(map[string]Property{
				"rig":  {Type: "string", Description: "rig id (default: the serving rig)"},
				"tail": {Type: "integer", Description: "lines to return (default 100)"},
			}),
			Handler: d.logs,
		},
		{
			Name: "larri_orphans",
			Description: "List provider resources that local state does not account for, and what they cost per hour. " +
				"Read-only; destroys nothing.",
			Schema:  Object(nil),
			Handler: d.orphans,
		},
		{
			Name: "larri_up",
			Description: "Rent a GPU and serve a model on it. THIS SPENDS MONEY — it rents hardware billed by " +
				"the second until destroyed. Call larri_search_offers first to see the price. Returns the " +
				"hourly rate actually committed to.",
			Schema: Object(map[string]Property{
				"model":        {Type: "string", Description: "model reference"},
				"quantization": {Type: "string"},
				"context":      {Type: "integer"},
				"gpu":          {Type: "string"},
				"max_price":    {Type: "number", Description: "ceiling in $/hr; refuses above it"},
				"idle_timeout": {Type: "string", Description: "e.g. '30m'; destroys after this long unused"},
				"budget":       {Type: "number", Description: "spend ceiling in $; destroys on breach"},
				"runtime":      {Type: "string", Enum: []string{"vllm", "llamacpp", "ollama"}},
				"dry_run":      {Type: "boolean", Description: "select and price without renting"},
			}, "model"),
			Consequential: true,
			Exposure:      ExposeMCPOnly,
			Handler:       d.up,
		},
		{
			Name: "larri_down",
			Description: "Destroy a rig, confirm it is gone at the provider, and report total cost. " +
				"THIS DESTROYS RENTED HARDWARE and stops the billing.",
			Schema: Object(map[string]Property{
				"rig": {Type: "string", Description: "rig id (default: the billing rig)"},
			}),
			Consequential: true,
			Exposure:      ExposeMCPOnly,
			Handler:       d.down,
		},
		{
			Name: "larri_orphan_destroy",
			Description: "Destroy one unaccounted-for provider resource and confirm its absence. " +
				"THIS DESTROYS RENTED HARDWARE. Split from larri_orphans so listing is always safe.",
			Schema: Object(map[string]Property{
				"instance_id": {Type: "string", Description: "provider instance id, from larri_orphans"},
			}, "instance_id"),
			Consequential: true,
			Exposure:      ExposeMCPOnly,
			Handler:       d.orphanDestroy,
		},
	}
}

// ---- handlers ----------------------------------------------------------

type statusArgs struct {
	All bool `json:"all"`
}

func (d Deps) status(ctx context.Context, raw json.RawMessage) (any, error) {
	var a statusArgs
	_ = json.Unmarshal(raw, &a)

	rigs, err := d.Store.List()
	if err != nil {
		return nil, err
	}
	entries, _ := d.Store.Entries()
	now := time.Now()

	out := []map[string]any{}
	for _, r := range rigs {
		if !a.All && r.State.Terminal() {
			continue
		}
		row := map[string]any{
			"rig":      r.ID,
			"state":    string(r.State),
			"model":    r.Model.Ref,
			"served":   r.Model.ServedName,
			"runtime":  string(r.Runtime),
			"gpu":      r.Offer.GPUModel,
			"price_hr": r.Offer.PriceHr,
			"billable": r.State.Billable(),
		}
		if r.Instance != nil {
			row["instance"] = r.Instance.InstanceID
			row["provider"] = r.Instance.Provider
		}
		if entries != nil {
			c := state.CostFor(entries, r.ID, now)
			row["accrued_usd"] = round4(c.TotalUSD)
			row["ran"] = c.Ran.Round(time.Second).String()
		}
		// Why a past rig ended is the whole reason terminated rigs are kept.
		if r.End != nil {
			row["ended"] = map[string]any{
				"actor": string(r.End.Actor), "code": string(r.End.Code),
				"summary": r.End.Summary, "total_usd": round4(r.End.Cost.TotalUSD),
			}
		}
		out = append(out, row)
	}
	return map[string]any{"rigs": out, "count": len(out)}, nil
}

type planArgs struct {
	Model        string `json:"model"`
	Quantization string `json:"quantization"`
	Context      int    `json:"context"`
}

func (d planArgs) spec() core.ModelSpec {
	q := d.Quantization
	if q == "" {
		q = "fp16"
	}
	c := d.Context
	if c == 0 {
		c = 8192
	}
	return core.ModelSpec{
		Ref: d.Model, Source: core.SourceHuggingFace, ServedName: "planned",
		Quantization: q, ContextLen: c,
	}
}

func (d Deps) plan(ctx context.Context, raw json.RawMessage) (any, error) {
	var a planArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	o, err := d.NewOrchestrator("")
	if err != nil {
		return nil, err
	}
	sv, err := o.Offers(ctx, daemon.UpRequest{
		Criteria: core.Criteria{MinReliability: 0.90, DiskGB: 60},
		Model:    a.spec(), DiskGB: 60,
	})
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"model":               a.Model,
		"required_vram_bytes": sv.Plan.RequiredVRAMBytes,
		"required_vram_gb":    round2(float64(sv.Plan.RequiredVRAMBytes) / (1 << 30)),
		"fits_in_vram":        sv.Plan.FitsInVRAM,
		"context_len":         sv.Plan.ContextLen,
		"warnings":            sv.Plan.Warnings,
		"offers_considered":   sv.Offers,
	}
	if sv.Selection.Selected != nil {
		c := sv.Selection.Selected.Offer
		res["cheapest"] = map[string]any{
			"gpu": c.GPUModel, "vram_gb": c.VRAMTotalGB(),
			"price_hr": c.PriceHr, "provider": c.Provider,
		}
		res["estimated_usd_per_hour"] = c.PriceHr
	}
	return res, nil
}

type offersArgs struct {
	planArgs
	GPU      string  `json:"gpu"`
	MaxPrice float64 `json:"max_price"`
	Top      int     `json:"top"`
}

func (d Deps) searchOffers(ctx context.Context, raw json.RawMessage) (any, error) {
	var a offersArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if a.Top <= 0 {
		a.Top = 10
	}
	o, err := d.NewOrchestrator("")
	if err != nil {
		return nil, err
	}
	crit := core.Criteria{MaxPriceHr: a.MaxPrice, MinReliability: 0.90, DiskGB: 60}
	if a.GPU != "" {
		crit.GPUModel = []string{a.GPU}
	}
	sv, err := o.Offers(ctx, daemon.UpRequest{Criteria: crit, Model: a.spec(), DiskGB: 60})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, c := range sv.Selection.Candidates {
		if !c.Eligible() || len(rows) >= a.Top {
			continue
		}
		rows = append(rows, map[string]any{
			"gpu": c.Offer.GPUModel, "vram_gb": c.Offer.VRAMTotalGB(),
			"price_hr": c.Offer.PriceHr, "reliability": c.Offer.Reliability,
			"provider": c.Offer.Provider, "offer_id": c.Offer.OfferID,
		})
	}
	return map[string]any{
		"offers": rows, "considered": sv.Offers,
		"required_vram_gb": round2(float64(sv.Plan.RequiredVRAMBytes) / (1 << 30)),
		"note":             "nothing was spent; larri_up rents the first of these",
	}, nil
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
func round4(f float64) float64 { return float64(int(f*10000+0.5)) / 10000 }
