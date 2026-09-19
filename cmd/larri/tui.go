// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/notice"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/term"
)

// cmdTUI brings a rig up under a dashboard.
//
// It is `up` with a different face, and deliberately not a different engine:
// the same orchestrator, the same supervisor, the same teardown. A TUI that ran
// its own lifecycle would be a second implementation of the part that spends
// money, which is the last thing that should exist twice.
func cmdTUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	model := fs.String("model", "", "model reference, e.g. Qwen/Qwen3-Coder-30B")
	served := fs.String("served-name", "", "stable name clients use (default: derived)")
	quant := fs.String("quantization", "", "fp16, Q4_K_M, awq, … (default: the runtime's)")
	ctxLen := fs.Int("context", 8192, "context length")
	gpu := fs.String("gpu", "", "GPU model filter")
	maxPrice := fs.Float64("max-price", 0, "ceiling in $/hr")
	allowLowStock := fs.Bool("allow-low-stock", false,
		"consider offers the provider reports at low stock; a create against one may be refused")
	disk := fs.Int("disk", 0, "disk in GB (0: sized to the model's weights, at least 60)")
	minRel := fs.Float64("min-reliability", 0.90, "reliability floor")
	port := fs.Int("port", 8000, "fixed local port clients are wired against")
	engine := fs.String("runtime", "", "vllm, llamacpp or ollama")
	idleFor := fs.Duration("idle-timeout", 0, "reclaim after this long without operator inference")
	idleAct := fs.String("idle-action", "", "destroy or warn")
	budget := fs.Float64("budget", 0, "spend ceiling in $")
	sshTimeout := fs.Duration("ssh-timeout", 0,
		"how long to wait for the published SSH endpoint (0: 3m default)")
	_ = fs.Parse(args)
	if *sshTimeout < 0 {
		return errors.New("ssh-timeout must not be negative")
	}

	if *model == "" {
		return errors.New("--model is required")
	}
	cfg := config.Default()
	if *idleFor != 0 {
		cfg.Idle.Timeout = *idleFor
	}
	if *idleAct != "" {
		cfg.Idle.Action = config.IdleAction(*idleAct)
	}
	cfg.Budget.MaxUSD = *budget
	if err := cfg.Validate(); err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := refuseIfAlreadyBilling(st); err != nil {
		return err
	}

	name := *served
	if name == "" {
		name = servedNameFor(*model)
	}
	spec := core.ModelSpec{
		Ref: *model, Source: core.SourceHuggingFace, ServedName: name,
		Quantization: *quant, ContextLen: *ctxLen,
	}
	if isOllamaRef(*model) {
		spec.Source = core.SourceOllamaRegistry
	}

	// The privacy notice is printed before the alternate screen opens, so it
	// is still on the scrollback afterwards. A warning that vanishes with the
	// dashboard was never really shown (FR-CFG-03).
	fmt.Println(notice.PrivacyFull())
	fmt.Printf("\n  %s\n\n", describePolicy(cfg))

	events := make(chan daemon.Event, 256)
	o, err := newOrchestrator(st, *engine, "", spec, events)
	if err != nil {
		return err
	}
	// The ceiling covers bring-up and not only what follows READY. Without
	// this a --budget on the dashboard bounded the supervisor alone, so a rig
	// could exhaust it during the weight download — which on a slow link is
	// most of what the operator pays for — and reach READY already over.
	o.BudgetUSD = cfg.Budget.MaxUSD
	if r, err := pickRuntime(*engine, spec); err == nil {
		o.Runtime = r
		for _, n := range securityNotes(r) {
			fmt.Printf("  ! runtime    %s\n\n", n)
		}
	} else {
		return err
	}
	o.Policy.ReliabilityFloor = *minRel
	sshWait := cfg.SSHTimeout
	if *sshTimeout != 0 {
		sshWait = *sshTimeout
	}
	o.EndpointStallLimit, o.AuthStallTimeout = sshWait, sshWait

	m := newTUIModel()
	prog := term.NewProgram(m, term.WithAltScreen(), term.WithContext(ctx))

	// Events reach the screen rather than stdout, which the alternate screen
	// owns for the duration.
	go func() {
		for e := range events {
			if !e.Show() {
				continue
			}
			prog.Send(tuiEvent(e))
		}
	}()
	defer close(events)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	crit := core.Criteria{MaxPriceHr: *maxPrice, MinReliability: *minRel, DiskGB: *disk,
		AllowLowStock: *allowLowStock}
	if *gpu != "" {
		crit.GPUModel = splitList(*gpu)
	}
	go runRig(runCtx, cancel, prog, o, st, cfg, rigRequest{
		spec: spec, port: *port, disk: *disk, crit: crit,
	})

	final, err := prog.Run()
	if err != nil && !errors.Is(err, term.ErrNotATerminal) {
		return err
	}
	// Reprint the outcome outside the alternate screen, so the cost survives
	// the dashboard closing.
	if fm, ok := final.(interface{ Summary() string }); ok {
		fmt.Print(fm.Summary())
	}
	return nil
}

type rigRequest struct {
	spec core.ModelSpec
	crit core.Criteria
	port int
	disk int
}

// runRig performs the lifecycle and reports it to the screen.
func runRig(ctx context.Context, cancel context.CancelFunc, prog *term.Program,
	o *daemon.Orchestrator, st *state.Store, cfg config.Config, req rigRequest) {

	live, err := o.UpAndServe(ctx, daemon.UpRequest{
		Criteria: req.crit, Model: req.spec, DiskGB: req.disk,
		HFToken: secret.New(os.Getenv("HF_TOKEN")), LocalPort: req.port,
	})
	if err != nil {
		prog.Send(tuiDone(nil, core.CostSummary{}, err))
		return
	}
	rig := live.Rig
	line, kerr := keyLine(live, openClientKeys())
	if kerr != nil {
		line = kerr.Error()
	}
	prog.Send(tuiReady(rig, live.Endpoint, line))

	// Sampling is what keeps the dashboard honest: cost comes from the
	// journal and activity from the proxy, so nothing on screen is a number
	// the TUI invented.
	sampleCtx, stopSampling := context.WithCancel(ctx)
	go sampleInto(sampleCtx, prog, o, live)

	term := o.Supervise(ctx, live, daemon.SupervisePolicy{Idle: cfg.Idle, Budget: cfg.Budget})
	stopSampling()

	release := live.EndServing()
	dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer dcancel()
	derr := o.Down(dctx, rig, term)
	release()

	cost := core.CostSummary{}
	if rig.End != nil {
		cost = rig.End.Cost
	}
	prog.Send(tuiDone(term, cost, derr))
	cancel()
}
