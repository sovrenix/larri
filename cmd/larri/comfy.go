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
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/comfy"
	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/workflow"
)

// cmdComfy rents hardware for a ComfyUI workflow and publishes it locally.
//
// The command is a session rather than a server: it brings the rig up, hands
// the operator a URL, holds the tunnel while they work, collects what they
// rendered, and destroys the host. `up` holds a tunnel for the same reason —
// a tunnel is a live process, and exiting while the rig bills would make the
// command look successful and cost money.
func cmdComfy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("comfy", flag.ExitOnError)
	wfPath := fs.String("workflow", "", "ComfyUI workflow JSON (the API format, from Save (API))")
	manifestPath := fs.String("models", "", "manifest mapping model filenames to repositories")
	outDir := fs.String("output", "", "local directory to save renders into (default: ./comfy-output)")
	port := fs.Int("port", 8188, "fixed local port the browser connects to")
	gpu := fs.String("gpu", "", "GPU model filter, e.g. 'RTX 4090'")
	maxPrice := fs.Float64("max-price", 0, "ceiling in $/hr")
	minRel := fs.Float64("min-reliability", 0.90, "reliability floor")
	minNet := fs.Float64("min-netspeed", 200, "minimum host download link, Mbps (0 disables)")
	// The bundle is downloaded at the rig's hourly rate, so how long the rig
	// will be used is what decides whether a cheap slow host or a dearer fast
	// one is the better buy (§4b).
	session := fs.Float64("session", 2, "hours of use to optimise the choice for")
	disk := fs.Int("disk", 0, "disk in GB (default: derived from the bundle)")
	// Off by default. A .ckpt or .pth is a pickle: torch executes it on load,
	// on the host holding the operator's Hugging Face token.
	allowPickle := fs.Bool("allow-pickle", false,
		"permit .ckpt/.pth models, which execute code when loaded")
	verifiedOnly := fs.Bool("verified-only", false, "rent only hosts the provider has verified")
	allowDeverified := fs.Bool("allow-deverified", false, "include hosts whose verification was withdrawn")
	bringupCap := fs.Duration("deadline", 90*time.Minute, "ceiling on one bring-up attempt")
	idleFor := fs.Duration("idle-timeout", 0, "reclaim after this long without rendering (0: use the default)")
	budget := fs.Float64("budget", 0, "spend ceiling in $; destroys on breach after a warning")
	image := fs.String("image", "", "container image to rent (default: the adapter's)")
	comfyRef := fs.String("comfyui-version", "",
		"ComfyUI git tag to install (default: the adapter's pinned one)")
	yes := fs.Bool("yes", false, "do not prompt before spending")
	dryRun := fs.Bool("dry-run", false, "parse, measure and rank without spending")
	providerName := fs.String("provider", "", "which provider to rent from")
	_ = fs.Parse(args)

	if *wfPath == "" {
		return errors.New("no workflow: pass --workflow <file.json>")
	}
	if *outDir == "" {
		*outDir = "comfy-output"
	}

	// ---- everything knowable before the money ----------------------------
	//
	// §4a in one block. The graph is read, its models are resolved to real
	// repositories and measured, and the VRAM requirement follows from both.
	// Each of these is free to get wrong here and expensive to get wrong after
	// a create call.
	raw, err := os.ReadFile(*wfPath)
	if err != nil {
		return fmt.Errorf("comfy: read workflow: %w", err)
	}
	graph, err := workflow.Parse(raw)
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(filepath.Base(*wfPath), filepath.Ext(*wfPath))

	fmt.Printf("  workflow    %s — %d nodes, %s serialisation\n",
		name, graph.Nodes, graph.Format)
	if !graph.Format.Executable() {
		// Said before anything is rented, because it changes what the session
		// can do: /prompt takes the API format and nothing else.
		fmt.Printf("  ! workflow   this is the ui serialisation: larri can size and rent for it,\n")
		fmt.Printf("               but cannot submit it. re-export with Save (API) to render.\n")
	}
	if graph.Image.Width > 0 {
		fmt.Printf("  render      %dx%d", graph.Image.Width, graph.Image.Height)
		if graph.Image.Batch > 1 {
			fmt.Printf(" x%d", graph.Image.Batch)
		}
		if graph.Steps > 0 {
			fmt.Printf(", %d steps", graph.Steps)
		}
		fmt.Println()
	}

	var manifest *workflow.Manifest
	if *manifestPath != "" {
		if manifest, err = workflow.LoadManifest(*manifestPath); err != nil {
			return err
		}
	}
	hf := secret.New(os.Getenv("HF_TOKEN"))
	sizer := workflow.NewHFSizer(hf)

	rctx, cancelResolve := context.WithTimeout(ctx, 90*time.Second)
	bundle, err := workflow.Resolve(rctx, graph, sizer, workflow.ResolveOptions{
		Manifest: manifest, AllowPickle: *allowPickle,
	})
	cancelResolve()
	if err != nil {
		return err
	}
	urls := make(map[string]string, len(bundle.Items))
	for _, it := range bundle.Items {
		urls[it.Asset.Name] = sizer.DownloadURL(it.Source)
	}

	fmt.Printf("  models      %d files, %s\n",
		len(bundle.Items), sizing.HumanBytes(bundle.TotalBytes))
	for _, it := range bundle.Items {
		fmt.Printf("              %-10s %s (%s)\n",
			it.Asset.Kind, it.Asset.Name, sizing.HumanBytes(it.Bytes))
	}

	plan, err := sizing.PlanDiffusion(sizing.DiffusionRequest{
		WeightBytes: bundle.TotalBytes, Pixels: bundle.Image.Pixels(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("  vram        ~%s (%s weights + %s working set)\n",
		sizing.HumanBytes(plan.RequiredVRAMBytes),
		sizing.HumanBytes(plan.WeightsBytes), sizing.HumanBytes(plan.KVCacheBytes))

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
	cfg := res.Config
	if *idleFor != 0 {
		cfg.Idle.Timeout = *idleFor
	}
	if *budget > 0 {
		cfg.Budget.MaxUSD = *budget
	}
	if err := cfg.Validate(); err != nil {
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
	fmt.Printf("  labels      %s — %s\n", keySrc, config.LabelKeyNotice(keySrc))

	absOut, err := filepath.Abs(*outDir)
	if err != nil {
		return err
	}
	// The image and the ComfyUI ref have to work *together*, so both are
	// stated before the confirmation. A live run proved what an unpinned pair
	// costs: torch 2.4 could not load ComfyUI's current custom ops, and the
	// failure arrived after the weights download rather than before it.
	fmt.Printf("  comfyui     %s\n", comfyVersionLabel(*comfyRef))
	fmt.Printf("  output      %s\n\n", absOut)

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
		IdleTimeout: cfg.Idle.Timeout,
		// The ceiling binds during bring-up too, not only once the rig is
		// READY and Supervise takes over.
		BudgetUSD:       cfg.Budget.MaxUSD,
		DeadmanDeadline: 0,
	}

	crit := core.Criteria{
		MaxPriceHr: *maxPrice, MinReliability: *minRel, DiskGB: *disk,
		MinNetMbps: *minNet, CertifiedOnly: *verifiedOnly,
		AllowDeverified: *allowDeverified,
	}
	if *gpu != "" {
		crit.GPUModel = splitList(*gpu)
	}

	req := daemon.ComfyRequest{
		Name: name, Graph: graph, Bundle: bundle, URLs: urls,
		Criteria: crit, LocalPort: *port, HFToken: hf,
		OutputDir: absOut, SessionHours: *session, AllowPickle: *allowPickle,
		Image: *image, ComfyUIRef: *comfyRef,
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

	if *image != "" {
		// Applied through the request rather than mutated afterwards, so the
		// image the floors were derived from is the image that is rented.
		//
		// Warned about, because the compute-capability and CUDA floors belong
		// to the adapter's own image and an override silently invalidates
		// them. That is the mistake that rented three V100 boxes on the vLLM
		// path: the floor still said 7.0 while the image had dropped Volta.
		fmt.Printf("  ! image      %s — the hardware floors were derived from a\n", *image)
		fmt.Printf("               different build; re-derive them if this one differs\n")
	}

	sess, err := o.ComfyUp(ctx, req)
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
	fmt.Printf("\n    open this once to log the browser in:\n      %s\n", sess.URL)
	fmt.Printf("\n  %s\n", daemon.PrivacyNotice(rig))
	fmt.Printf("\n  %s\n", describePolicy(cfg))
	fmt.Printf("  renders are saved to %s on teardown\n", absOut)
	fmt.Printf("  holding the session — Ctrl-C to collect and stop paying\n\n")

	term := o.Supervise(ctx, sess.Live, daemon.SupervisePolicy{
		Idle: cfg.Idle, Budget: cfg.Budget,
	})
	if term == nil {
		fmt.Printf("\n  interrupted; collecting renders and tearing down\n")
		term = &core.Termination{
			Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
			At: time.Now().UTC(), Summary: "session ended from the CLI",
		}
	} else {
		fmt.Printf("\n  ! %s — %s\n", term.Code, term.Summary)
	}

	// The context may already be cancelled by the interrupt that got us here,
	// and both halves of this still have to run: collection needs a live SSH
	// session, and the destroy is the one thing that must never be skipped.
	tctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	sync, err := o.ComfyDown(tctx, sess, term)
	if sync != nil {
		fmt.Printf("\n  renders     %s in %s\n", sync.Summary(), sync.Dir)
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

// comfyVersionLabel names the application version that will be installed.
//
// Printed before the confirmation because the image and this ref have to work
// together, and a live run proved what an unpinned pair costs: torch 2.4 could
// not load ComfyUI's current custom ops, and the failure arrived after the
// weights download rather than before it.
func comfyVersionLabel(flagged string) string {
	if flagged != "" {
		return flagged + " (--comfyui-version)"
	}
	return comfy.DefaultComfyUIRef
}
