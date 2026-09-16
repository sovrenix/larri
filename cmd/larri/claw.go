// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	_ "go.sovrenix.com/larri/internal/claw/comfyui"
	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
)

// cmdClaw rents hardware for an application and holds it open.
//
// One command for every application rather than one per type, and the flags
// here are the ones every claw honours: the market, the lifecycle, the safety
// ceilings. Anything specific to a type lives in its job file, because a
// generic command that grows a --workflow the day something wants one is not a
// generic command.
//
// The session is held in the foreground for the reason `up` is: a tunnel is a
// live process, and exiting while the rig bills would make the command look
// successful and cost money.
func cmdClaw(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("claw", flag.ExitOnError)
	list := fs.Bool("list", false, "list the claw types compiled in")
	cfgPath := fs.String("config", "", "claw job file")
	typeName := fs.String("type", "", "claw type, when the job file does not say")
	outDir := fs.String("output", "", "where a remote claw's results are saved (default: ./claw-output)")
	port := fs.Int("port", 0, "fixed local port the session is published on")

	gpu := fs.String("gpu", "", "GPU model filter, e.g. 'RTX 4090'")
	maxPrice := fs.Float64("max-price", 0, "ceiling in $/hr")
	minRel := fs.Float64("min-reliability", 0.90, "reliability floor")
	minNet := fs.Float64("min-netspeed", 200, "minimum host download link, Mbps (0 disables)")
	disk := fs.Int("disk", 0, "disk in GB (default: whatever the claw asks for)")
	verifiedOnly := fs.Bool("verified-only", false, "rent only hosts the provider has verified")
	allowLowStock := fs.Bool("allow-low-stock", false,
		"consider offers the provider reports at low stock; a create against one may be refused")
	allowDeverified := fs.Bool("allow-deverified", false, "include hosts whose verification was withdrawn")
	providerName := fs.String("provider", "", "which provider to rent from")

	// How long the rig will be used decides whether a cheap slow host or a
	// dearer fast one is the better buy: whatever the claw downloads is billed
	// at the rig's hourly rate (§4b).
	session := fs.Float64("session", 2, "hours of use to optimise the choice for")
	bringupCap := fs.Duration("deadline", 90*time.Minute, "ceiling on one bring-up attempt")
	idleFor := fs.Duration("idle-timeout", 0, "reclaim after this long without work (0: use the default)")
	budget := fs.Float64("budget", 0, "spend ceiling in $; destroys on breach after a warning")

	yes := fs.Bool("yes", false, "do not prompt before spending")
	dryRun := fs.Bool("dry-run", false, "plan and rank without spending")
	_ = fs.Parse(args)

	if *list {
		return listClaws()
	}
	if *cfgPath == "" {
		return errors.New("no job file: pass --config <file.yml>, or --list to see the types")
	}

	// ---- everything knowable before the money ----------------------------
	//
	// The claw reads its own configuration and says what hardware that
	// implies, all locally. §4a: a precondition establishable without renting
	// must be, because the alternative is paying to discover it.
	cfg, err := claw.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.WithType(claw.Type(*typeName)); err != nil {
		return err
	}
	kind, err := claw.Open(cfg.Type)
	if err != nil {
		return err
	}

	out := *outDir
	switch kind.Site() {
	case claw.SiteRemote:
		if out == "" {
			out = "claw-output"
		}
		if out, err = filepath.Abs(out); err != nil {
			return err
		}
	case claw.SiteLocal:
		if *outDir != "" {
			// A flag that silently does nothing is worse than one that is
			// refused: the operator would believe results were being saved.
			return fmt.Errorf("claw: %s runs locally: --output has nothing to collect", cfg.Type)
		}
	}

	crit := core.Criteria{
		MaxPriceHr: *maxPrice, MinReliability: *minRel, DiskGB: *disk,
		MinNetMbps: *minNet, CertifiedOnly: *verifiedOnly,
		AllowDeverified: *allowDeverified, AllowLowStock: *allowLowStock,
	}
	if *gpu != "" {
		crit.GPUModel = splitList(*gpu)
	}
	hf := secret.New(os.Getenv("HF_TOKEN"))

	pctx, cancelPlan := context.WithTimeout(ctx, 2*time.Minute)
	plan, err := kind.Plan(pctx, cfg, claw.Options{
		Criteria: crit, OutputDir: out, HFToken: hf, SessionHours: *session,
	})
	cancelPlan()
	if err != nil {
		return err
	}

	// The header only. Summary and Caveats belong to the daemon, which emits
	// them to every front-end before the confirmation — printing our own copy
	// as well showed the operator the same eight lines twice.
	fmt.Printf("  claw        %s (%s) — %s\n", cfg.Type, kind.Site(), kind.Describe())
	fmt.Printf("  config      %s\n", cfg.Path)
	if plan.Sizing != nil {
		fmt.Printf("  vram        ~%s\n", sizing.HumanBytes(plan.Sizing.RequiredVRAMBytes))
	}
	if plan.ColdStartBytes > 0 {
		fmt.Printf("  download    %s before it can work\n", sizing.HumanBytes(plan.ColdStartBytes))
	}
	if kind.Site() == claw.SiteRemote {
		fmt.Printf("  output      %s\n", out)
	}

	// ---- the rest of the setup -------------------------------------------
	prov, err := openProvider(*providerName)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	if !*dryRun {
		// Renting a second rig while a first one is still billing is the leak
		// this product exists to prevent.
		if err := refuseIfAlreadyBilling(st); err != nil {
			return err
		}
	}

	res, err := config.Resolve(config.Request{})
	if err != nil {
		return err
	}
	conf := res.Config
	if *idleFor != 0 {
		conf.Idle.Timeout = *idleFor
	}
	if *budget > 0 {
		conf.Budget.MaxUSD = *budget
	}
	if err := conf.Validate(); err != nil {
		return err
	}

	labelKey, keySrc, err := config.ResolveLabelKey(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	sealer, err := config.LabelSealer(labelKey)
	if err != nil {
		return err
	}
	fmt.Printf("  labels      %s — %s\n\n", keySrc, config.LabelKeyNotice(keySrc))

	events := make(chan daemon.Event, 64)
	prompts := make(chan cliPrompt)
	go printCLIOutput(events, prompts)
	defer close(events)

	provider.Report(prov,
		func(e error) { fmt.Printf("  ! drift      %v\n", e) },
		func(m string) { fmt.Printf("  ! search     %s\n", m) })

	o := &daemon.Orchestrator{
		Store: st, Provider: prov,
		LabelSealer: sealer,
		Policy: rank.Policy{
			ReliabilityFloor: *minRel,
			OutlierFactor:    rank.DefaultPolicy().OutlierFactor,
			MinClassSample:   rank.DefaultPolicy().MinClassSample,
			SessionHours:     *session,
		},
		Deadline:    *bringupCap,
		Events:      events,
		IdleTimeout: conf.Idle.Timeout,
		BudgetUSD:   conf.Budget.MaxUSD,
	}

	req := daemon.ClawRequest{
		Kind: kind, Plan: plan, LocalPort: *port,
		HFToken: hf, OutputDir: out,
	}
	mode := config.DetectMode(config.Invocation{ForceNonInteractive: *yes}, os.Getenv)
	switch {
	case *dryRun:
		req.Confirm = func(of core.Offer, _ core.SizingPlan) bool {
			fmt.Printf("\n  dry run: would rent %s %s at $%.3f/hr — nothing spent\n",
				of.Provider, of.GPUModel, of.PriceHr)
			return false
		}
	case mode.Interactive():
		req.Confirm = func(of core.Offer, _ core.SizingPlan) bool {
			result := make(chan bool)
			prompts <- cliPrompt{Offer: of, Result: result}
			return <-result
		}
	}

	sess, err := o.ClawUp(ctx, req)
	if *dryRun && errors.Is(err, daemon.ErrConfirmationDeclined) {
		return nil
	}
	if err != nil {
		return err
	}

	rig := sess.Live.Rig
	fmt.Printf("\n  ✓ rig %s READY\n", rig.ID)
	fmt.Printf("    %s %s at $%.3f/hr\n",
		rig.Offer.Provider, rig.Offer.GPUModel, rig.Offer.PriceHr)
	if sess.URL != "" {
		fmt.Printf("\n    open this once to log the browser in:\n      %s\n", sess.URL)
	} else {
		fmt.Printf("    endpoint %s\n", sess.Endpoint())
	}
	fmt.Printf("\n  %s\n", daemon.PrivacyNotice(rig))
	fmt.Printf("\n  %s\n", describePolicy(conf))
	if kind.Site() == claw.SiteRemote {
		fmt.Printf("  results are saved to %s on teardown\n", out)
	}
	fmt.Printf("  holding the session — Ctrl-C to stop paying\n\n")

	term := o.Supervise(ctx, sess.Live, daemon.SupervisePolicy{
		Idle: conf.Idle, Budget: conf.Budget,
	})
	if term == nil {
		fmt.Printf("\n  interrupted; tearing down\n")
		term = &core.Termination{
			Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
			At: time.Now().UTC(), Summary: "session ended from the CLI",
		}
	} else {
		fmt.Printf("\n  ! %s — %s\n", term.Code, term.Summary)
	}

	// A fresh context: the interrupt that got us here has already cancelled
	// the one above, and both halves of the teardown still have to run — the
	// destroy especially.
	tctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	collected, err := o.ClawDown(tctx, sess, term)
	if collected != nil {
		fmt.Printf("\n  results     %s in %s\n", collected.Summary(), collected.Dir)
	}
	if err != nil {
		return err
	}
	fmt.Printf("  ✓ destroyed and confirmed absent\n")
	if term.Cost.TotalUSD > 0 {
		fmt.Printf("    spent $%.4f over %s\n",
			term.Cost.TotalUSD, term.Cost.Ran.Round(time.Second))
	}
	return nil
}

// listClaws prints what is compiled in.
//
// The site is shown because it is the fact that changes what an operator gets:
// a session they open in a browser and whose results come back at teardown, or
// an application they already run being pointed at the rig.
func listClaws() error {
	lines := claw.Describe()
	if len(lines) == 0 {
		fmt.Println("  no claw types are compiled in")
		return nil
	}
	fmt.Println("  type         site   what it is")
	for _, l := range lines {
		fmt.Printf("  %s\n", l)
	}
	return nil
}
