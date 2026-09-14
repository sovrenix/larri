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
	"go.sovrenix.com/larri/internal/secret"
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
	//
	// providerName is the provider holding an existing rig, or "" for the
	// configured default when there is no rig yet. Acting on a rig through
	// any other provider reads "not found" as confirmed absence.
	//
	// model is what the engine is chosen from when runtimeKind is empty, the
	// same reading the CLI gives it. Chosen from nothing, a named .gguf file
	// or a Q4_K_M request got vLLM, which cannot load either.
	NewOrchestrator func(runtimeKind, providerName string, model core.ModelSpec) (*daemon.Orchestrator, error)

	// Providers names every provider an orphan could be at. An orphan is
	// something LARRI lost track of, so which provider holds it is not known
	// in advance, and listing only the default hid the rest.
	Providers func() []string

	// Session is the rig this surface is holding, when it is long-running
	// enough to hold one. An MCP server outlives its tool calls, so it can;
	// a one-shot CLI invocation cannot, and leaves this nil.
	Session *Session

	// HFToken is the weight-download credential, handed to a bring-up at
	// launch so it never reaches a snapshot or a journal entry (FR-STATE-05).
	HFToken secret.Secret
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
				"Also reports the rig this server is bringing up or serving, including its progress and — " +
				"once ready — the OpenAI-compatible endpoint and api key to send requests to. " +
				"Poll this after larri_up. Read-only; spends nothing.",
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
				"gpus":         {Type: "integer", Description: "minimum GPUs per host"},
				"max_gpus":     {Type: "integer", Description: "maximum GPUs per host"},
				"allow_low_stock": {Type: "boolean", Description: "consider offers the provider reports at low stock; " +
					"a create against one may be refused, and larri falls back if it is"},
				"vram":         {Type: "integer", Description: "minimum aggregate VRAM in GB"},
				"vram_per_gpu": {Type: "integer", Description: "minimum VRAM per card in GB"},
				"max_price":    {Type: "number", Description: "ceiling in $/hr"},
				"top":          {Type: "integer", Description: "how many to return (default 10)"},
			}, "model"),
			Handler: d.searchOffers,
		},
		{
			Name: "larri_logs",
			Description: "Read a rig's log. For the rig this server is serving, the inference engine's own log " +
				"(source: runtime) — what a launch is doing, or why it failed; available once SSH is up. For a rig " +
				"another larri process holds, such as one started with larri up -d, larri's own log of it " +
				"(source: larri) — bring-up, supervision and teardown.",
			Schema: Object(map[string]Property{
				"rig":  {Type: "string", Description: "rig id or a unique prefix (default: the serving rig, else the newest rig with a log)"},
				"tail": {Type: "integer", Description: "lines to return (default 100)"},
			}),
			Handler: d.logs,
		},
		{
			Name: "larri_orphans",
			Description: "List provider resources that local state does not account for, at every provider, and what they cost per hour. " +
				"Read-only; destroys nothing.",
			Schema:  Object(nil),
			Handler: d.orphans,
		},
		{
			Name: "larri_up",
			Description: "Rent a GPU and serve a model on it. THIS SPENDS MONEY — it rents hardware billed " +
				"by the second from the moment an instance exists, until destroyed. Call larri_search_offers " +
				"first to see the price. Returns immediately: bring-up takes minutes (image pull, weight " +
				"download, model load), so poll larri_status until it reports READY and an endpoint. Stop it " +
				"at any point with larri_down.",
			Schema: Object(map[string]Property{
				"model":           {Type: "string", Description: "model reference"},
				"quantization":    {Type: "string"},
				"context":         {Type: "integer"},
				"gpu":             {Type: "string"},
				"gpus":            {Type: "integer"},
				"max_gpus":        {Type: "integer"},
				"allow_low_stock": {Type: "boolean", Description: "consider offers at low stock; a create may be refused"},
				"vram":            {Type: "integer"},
				"vram_per_gpu":    {Type: "integer"},
				"max_price":       {Type: "number", Description: "ceiling in $/hr; refuses above it"},
				"idle_timeout":    {Type: "string", Description: "e.g. '30m'; destroys after this long unused"},
				"budget":          {Type: "number", Description: "spend ceiling in $; destroys on breach"},
				"runtime":         {Type: "string", Enum: []string{"vllm", "llamacpp", "ollama"}},
				"dry_run":         {Type: "boolean", Description: "select and price without renting"},
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
				"provider":    {Type: "string", Description: "the provider holding it, from larri_orphans"},
			}, "instance_id", "provider"),
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
		// The same summary `larri status` renders, so an agent and an
		// operator asking about one rig get one answer.
		sm := d.Store.Describe(r, entries, now)
		row := map[string]any{
			"rig":       sm.ID,
			"state":     string(sm.State),
			"provider":  sm.Provider,
			"hardware":  sm.Hardware,
			"gpu":       sm.GPUModel,
			"gpu_count": sm.GPUCount,
			"vram_gb":   sm.VRAMGB,
			"model":     sm.Model,
			"quant":     sm.Quantization,
			"served":    sm.Served,
			"runtime":   string(sm.Runtime),
			"price_hr":  round4(sm.PriceHr),
			"quoted_hr": round4(sm.QuotedHr),
			"billable":  sm.State.Billable(),
			"held":      sm.Held,
			"created":   sm.CreatedAt.UTC().Format(time.RFC3339),
		}
		if sm.Instance != "" {
			row["instance"] = sm.Instance
		}
		if sm.Region != "" {
			row["region"] = sm.Region
		}
		if sm.Endpoint != "" {
			row["endpoint"] = sm.Endpoint
		}
		if entries != nil {
			row["accrued_usd"] = round4(sm.Cost.TotalUSD)
			row["ran"] = sm.Cost.Ran.Round(time.Second).String()
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
	res := map[string]any{"rigs": out, "count": len(out)}

	// The in-flight bring-up, which is the whole reason larri_up returns
	// early. Without this the agent has started something that bills and has
	// no way to ask what it is doing.
	if d.Session != nil {
		snap := d.Session.Snapshot()
		if snap.Running || snap.Endpoint != "" || snap.Err != nil {
			held := map[string]any{
				"phase":   snap.Phase,
				"detail":  snap.Message,
				"elapsed": snap.Elapsed.Round(time.Second).String(),
				"running": snap.Running,
			}
			if snap.RigID != "" {
				held["rig"] = snap.RigID
			}
			if snap.Endpoint != "" {
				held["endpoint"] = snap.Endpoint
				held["api_key"] = snap.Token
				held["ready"] = true
				held["usage"] = "OpenAI-compatible; POST " + snap.Endpoint +
					"/chat/completions with this api_key as a bearer token"
			}
			if snap.Err != nil {
				held["error"] = snap.Err.Error()
			}
			res["serving"] = held
		}
	}
	return res, nil
}

type planArgs struct {
	Model        string `json:"model"`
	Quantization string `json:"quantization"`
	Context      int    `json:"context"`
}

// spec leaves an unnamed quantisation empty: the daemon fills in the
// runtime's own default while sizing, and an "fp16" filled in here asked a
// GGUF engine for full precision.
func (d planArgs) spec() core.ModelSpec {
	c := d.Context
	if c == 0 {
		c = 8192
	}
	return core.ModelSpec{
		Ref: d.Model, Source: core.SourceHuggingFace, ServedName: "planned",
		Quantization: d.Quantization, ContextLen: c,
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
	o, err := d.NewOrchestrator("", "", a.spec())
	if err != nil {
		return nil, err
	}
	sv, err := o.Offers(ctx, daemon.UpRequest{
		Criteria: core.Criteria{MinReliability: 0.90},
		Model:    a.spec(),
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
			"price_hr": round4(c.PriceHr), "provider": c.Provider,
		}
		res["estimated_usd_per_hour"] = round4(c.PriceHr)
	}
	return res, nil
}

type offersArgs struct {
	planArgs
	GPU        string  `json:"gpu"`
	GPUCount   int     `json:"gpus"`
	MaxGPU     int     `json:"max_gpus"`
	LowStock   bool    `json:"allow_low_stock"`
	VRAMTotal  int     `json:"vram"`
	VRAMPerGPU int     `json:"vram_per_gpu"`
	MaxPrice   float64 `json:"max_price"`
	Top        int     `json:"top"`
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
	o, err := d.NewOrchestrator("", "", a.spec())
	if err != nil {
		return nil, err
	}
	crit := core.Criteria{
		MaxPriceHr:     a.MaxPrice,
		MinReliability: 0.90,
		GPUCount:       a.GPUCount,
		MaxGPUCount:    a.MaxGPU,
		AllowLowStock:  a.LowStock,
		VRAMTotalGB:    a.VRAMTotal,
		VRAMPerGPUGB:   a.VRAMPerGPU,
	}
	if a.GPU != "" {
		crit.GPUModel = []string{a.GPU}
	}
	sv, err := o.Offers(ctx, daemon.UpRequest{Criteria: crit, Model: a.spec()})
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
			"price_hr": round4(c.Offer.PriceHr), "reliability": round2(c.Offer.Reliability),
			"provider": c.Offer.Provider, "offer_id": c.Offer.OfferID,
			"low_stock": c.Offer.LowStock,
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
