// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/rank"
)

// cmdOffers ranks the market without spending anything.
//
// It runs the same sizing, the same fit test and the same ranking as `up`,
// through one shared path — a preview that ran its own search would eventually
// recommend an offer `up` rejects, and the operator would have no way to tell
// which of the two was lying.
func cmdOffers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("offers", flag.ExitOnError)
	model := fs.String("model", "", "model reference, e.g. Qwen/Qwen3-Coder-30B")
	quant := fs.String("quantization", "fp16", "fp16, q4_K_M, awq, ...")
	ctxLen := fs.Int("context", 8192, "context length")
	gpu := fs.String("gpu", "", "GPU model filter, e.g. 'RTX 4090'")
	maxPrice := fs.Float64("max-price", 0, "ceiling in $/hr")
	disk := fs.Int("disk", 60, "disk in GB")
	minRel := fs.Float64("min-reliability", 0.90, "reliability floor")
	engine := fs.String("runtime", "", "vllm, llamacpp or ollama (default: chosen from the model)")
	top := fs.Int("top", 10, "how many to show")
	providerName := fs.String("provider", "", "which provider to search (default: the only one compiled in)")
	_ = fs.Parse(args)

	if *model == "" {
		return errors.New("--model is required")
	}
	prov, err := openProvider(*providerName)
	if err != nil {
		return err
	}

	spec := core.ModelSpec{
		Ref: *model, Source: core.SourceHuggingFace, ServedName: "preview",
		Quantization: *quant, ContextLen: *ctxLen,
	}
	// The same reading `up` applies, because a preview that classified the
	// reference differently would rank a different market than the one `up`
	// rents from.
	if isOllamaRef(*model) {
		spec.Source = core.SourceOllamaRegistry
	}
	resolver, err := prepareSpec(ctx, &spec)
	if err != nil {
		return err
	}
	eng, err := pickRuntime(*engine, spec)
	if err != nil {
		return err
	}
	fmt.Printf("  runtime     %s (%s)\n", eng.Kind(), runtimeWhy(*engine, spec))

	events := make(chan daemon.Event, 64)
	go func() {
		for e := range events {
			mark := " "
			if e.Warning {
				mark = "!"
			}
			fmt.Printf("  %s %-10s %s\n", mark, e.Phase, e.Message)
		}
	}()
	defer close(events)

	p := prov
	provider.Report(p, nil, func(m string) { fmt.Printf("  ! search     %s\n", m) })

	o := &daemon.Orchestrator{
		Provider: p, Runtime: eng,
		Resolver: resolver,
		Policy: rank.Policy{
			ReliabilityFloor: *minRel,
			OutlierFactor:    rank.DefaultPolicy().OutlierFactor,
			MinClassSample:   rank.DefaultPolicy().MinClassSample,
		},
		Events: events,
	}
	crit := core.Criteria{MaxPriceHr: *maxPrice, MinReliability: *minRel, DiskGB: *disk}
	if *gpu != "" {
		crit.GPUModel = splitList(*gpu)
	}

	sv, err := o.Offers(ctx, daemon.UpRequest{Criteria: crit, Model: spec, DiskGB: *disk})
	if err != nil {
		return err
	}

	fmt.Printf("\n  %-4s %-18s %-6s %-10s %-6s %s\n", "", "gpu", "vram", "$/hr", "rel", "provider")
	rows := eligibleTop(sv.Selection, *top)
	for i, c := range rows {
		mark := "  "
		if sv.Selection.Selected != nil && c.Offer.OfferID == sv.Selection.Selected.Offer.OfferID {
			mark = "→ " // what `up` would rent right now
		}
		// An unreported score prints as a dash, not as 0.00. A catalogue
		// provider publishes none, and rendering that as zero reads as the
		// worst possible host rather than as "no data".
		rel := "—"
		if c.Offer.HasReliability() {
			rel = fmt.Sprintf("%.2f", c.Offer.Reliability)
		}
		fmt.Printf("  %s%-2d %-18s %-6s $%-9.3f %-6s %s\n",
			mark, i+1, c.Offer.GPUModel,
			fmt.Sprintf("%dGB", c.Offer.VRAMTotalGB()),
			c.Offer.PriceHr, rel, c.Offer.Provider)
	}
	if len(rows) == 0 {
		fmt.Println("\n  no offer satisfies the criteria")
	}
	fmt.Printf("\n  nothing was spent — 'larri up' with the same flags rents the marked offer\n")
	return nil
}

// cmdOrphans finds resources local state does not account for.
//
// This is the command for the case the whole product exists to prevent: a
// billing instance nobody is tracking. It lists by provider-side label rather
// than by local records, because the situation it addresses is precisely the
// one where the local records are gone.
func cmdOrphans(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("orphans", flag.ExitOnError)
	destroy := fs.Bool("destroy", false, "destroy every orphan found, confirming absence")
	yes := fs.Bool("yes", false, "do not prompt")
	orphanProvider := fs.String("provider", "", "which provider to sweep (default: the only one compiled in)")
	_ = fs.Parse(args)

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	prov, err := openProvider(*orphanProvider)
	if err != nil {
		return err
	}
	events := make(chan daemon.Event, 32)
	go func() {
		for e := range events {
			mark := " "
			if e.Warning {
				mark = "!"
			}
			fmt.Printf("  %s %-10s %s\n", mark, e.Phase, e.Message)
		}
	}()
	defer close(events)

	labelKey, _, err := config.ResolveLabelKey(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	sealer, err := config.LabelSealer(labelKey)
	if err != nil {
		return err
	}
	o := &daemon.Orchestrator{
		Store: st, Provider: prov,
		LabelSealer: sealer, Events: events,
	}

	orphans, err := o.Orphans(ctx)
	if err != nil {
		return err
	}
	if len(orphans) == 0 {
		fmt.Println("  no orphans — every larri resource is accounted for")
		return nil
	}

	var hourly float64
	fmt.Printf("\n  %d resource(s) local state does not account for:\n\n", len(orphans))
	for _, orph := range orphans {
		fmt.Printf("    %s\n", orph.Describe())
		hourly += orph.Instance.PriceHr
	}
	fmt.Printf("\n  costing $%.3f/hr in total\n", hourly)

	if !*destroy {
		fmt.Println("\n  destroy them with: larri orphans --destroy")
		return nil
	}
	mode := config.DetectMode(config.Invocation{ForceNonInteractive: *yes}, os.Getenv)
	if mode.Interactive() && !*yes {
		fmt.Printf("\n  destroy all %d? [y/N] ", len(orphans))
		var in string
		fmt.Scanln(&in)
		if !strings.EqualFold(strings.TrimSpace(in), "y") {
			fmt.Println("  left alone")
			return nil
		}
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	n, err := o.SweepOrphans(dctx)
	fmt.Printf("\n  destroyed %d of %d, absence confirmed\n", n, len(orphans))
	return err
}

// eligibleTop returns the offers worth showing, cheapest first.
//
// Candidate.Eligible() rather than a bare ReasonEligible test: the ranking
// marks the cheapest offer ReasonEligible and every runner-up ReasonCostlier,
// so testing for the former alone renders exactly one row and presents it as
// the whole market. That is what this did until a live run against 1376 offers
// returned a single line.
func eligibleTop(sel rank.Result, n int) []rank.Candidate {
	out := make([]rank.Candidate, 0, n)
	for _, c := range sel.Candidates {
		if !c.Eligible() {
			continue
		}
		if len(out) >= n {
			break
		}
		out = append(out, c)
	}
	return out
}
