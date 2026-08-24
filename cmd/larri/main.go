// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Command larri rents a GPU, serves a model on it, points local tools at it,
// and stops paying when told to.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.sovrenix.com/larri/internal/buildinfo"
	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/notice"
	"go.sovrenix.com/larri/internal/provider"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/runtime/vllm"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/state"
)

const usage = `larri — Local Agent for Remote Rigging of Inference

  larri up      rent a GPU, serve a model, wire local clients
  larri down    revert wiring, destroy the rig, confirm it is gone
  larri resume  rebuild the tunnel to a rig that outlived the last process
  larri offers  search and rank without spending anything
  larri orphans find and destroy resources local state does not account for
  larri config  edit saved criteria, previewing what they would rent
  larri tui     bring a rig up under a live dashboard
  larri mcp     expose the lifecycle as MCP tools for Claude Code and other agents
  larri status  what is running, what it costs, and why past rigs ended
  larri version build metadata

Run 'larri <command> -h' for the flags of each.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "up":
		err = cmdUp(ctx, os.Args[2:])
	case "down":
		err = cmdDown(ctx, os.Args[2:])
	case "resume":
		err = cmdResume(ctx, os.Args[2:])
	case "offers":
		err = cmdOffers(ctx, os.Args[2:])
	case "orphans":
		err = cmdOrphans(ctx, os.Args[2:])
	case "config":
		err = cmdConfig(ctx, os.Args[2:])
	case "tui":
		err = cmdTUI(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "status":
		err = cmdStatus(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(buildinfo.String())
		if buildinfo.Prerelease() {
			// Said rather than implied by omission: a pre-1.0 build is a
			// statement that the interface may still move, and anyone wiring
			// tools against it deserves to hear that from the tool.
			fmt.Println("pre-1.0: the command line and config format may still change")
		}
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "larri: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nlarri: %v\n", err)
		os.Exit(1)
	}
}

func stateDir() string {
	if d := os.Getenv("LARRI_STATE_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "larri")
}

func openStore() (*state.Store, error) { return state.Open(stateDir()) }

// firstRun prints the configuration LARRI adopted.
//
// FR-CFG-03: the destructive defaults are stated whether or not anyone is at a
// terminal to read them. A default that destroys and was never mentioned is a
// trap however well reasoned. The privacy notice rides along for the same
// reason — it is the single most consequential fact about the product, and it
// is not conditional on there being a human watching.
// firstRun states the destructive defaults when there is no configuration to
// state them from (FR-CFG-03).
//
// Conditional on there being no file, because once one exists the settings
// actually in force are disclosed individually by Resolution.Disclose — which
// is more precise, and printing both trains the operator to skim past the
// line that matters.
//
// The privacy notice is not conditional on anything. It is the single most
// consequential fact about the product and it prints on every run.
func firstRun(cfg config.Config, mode config.Mode, haveFile bool) {
	if !haveFile {
		fmt.Println("  defaults    no configuration file; adopting the built-in defaults")
		for _, d := range cfg.Disclosures() {
			mark := " "
			if d.Destructive {
				mark = "!"
			}
			fmt.Printf("            %s %-14s %-22s (%s)\n", mark, d.Setting, d.Value, d.ChangeWith)
		}
		fmt.Println()
	}
	fmt.Println(notice.PrivacyFull())
	fmt.Println()
	if !mode.Interactive() {
		fmt.Printf("  (running %s; proceeding on the above)\n\n", mode)
	}
}

func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	model := fs.String("model", "", "model reference, e.g. Qwen/Qwen3-Coder-30B")
	served := fs.String("served-name", "", "stable name clients use (default: derived)")
	quant := fs.String("quantization", "fp16", "fp16, q4_K_M, awq, ...")
	ctxLen := fs.Int("context", 8192, "context length")
	gpu := fs.String("gpu", "", "GPU model filter, e.g. 'RTX 4090'")
	maxPrice := fs.Float64("max-price", 0, "ceiling in $/hr")
	disk := fs.Int("disk", 60, "disk in GB")
	minRel := fs.Float64("min-reliability", 0.90, "reliability floor")
	port := fs.Int("port", 8000, "fixed local port clients are wired against")
	yes := fs.Bool("yes", false, "do not prompt before spending")
	dryRun := fs.Bool("dry-run", false, "search, size and select without spending")
	engine := fs.String("runtime", "", "vllm, llamacpp or ollama (default: chosen from the model)")
	idleFor := fs.Duration("idle-timeout", 0, "reclaim after this long without operator inference (0: use the default)")
	idleAct := fs.String("idle-action", "", "destroy or warn (default: destroy)")
	budget := fs.Float64("budget", 0, "spend ceiling in $; destroys on breach after a warning")
	profile := fs.String("profile", "", "saved profile to layer under the flags (default: the one named 'default')")
	providerName := fs.String("provider", "", "which provider to rent from (default: the only one compiled in)")
	deadman := fs.Duration("host-watchdog", 0,
		"how long the host waits without hearing from larri before stopping itself "+
			"(0: derive from --idle-timeout; -1: disable)")
	_ = fs.Parse(args)

	// Which flags the operator actually passed, so that a flag set to a value
	// equal to its default still beats the file. Without this, --max-price 0
	// could not turn a saved ceiling off.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	res, err := config.Resolve(config.Request{
		Profile: *profile, SetFlags: set,
		// First run writes a defaults file and says so. It never opens a
		// form: `up` is a command that spends, and an operator who asked to
		// rent hardware should not be interviewed first (FR-CFG-01).
		Ensure: config.DetectMode(config.Invocation{ForceNonInteractive: *yes}, os.Getenv).Interactive(),
	})
	if err != nil {
		return err
	}
	if res.Created {
		fmt.Printf("  config      created %s with the built-in defaults\n", config.Path())
		fmt.Printf("              edit it with: larri config\n")
	}
	applyProfile(res.Profile, set, model, quant, ctxLen, gpu, maxPrice, disk, minRel, port, engine)
	if res.Name != "" {
		// FR-CRIT-05 forbids *silently* reusing criteria. A named default
		// profile may apply to a bare `larri up` only because this line makes
		// it visible in full — not merely the parts that spend.
		fmt.Printf("  profile     %s — %s\n", res.Name, res.Profile.Summary())
	}
	for _, d := range res.Disclose {
		fmt.Printf("  ! %-12s %s from %s\n", d.Setting, d.Value, d.From)
	}

	// Checked after the profile is applied, because a profile that names a
	// model is precisely what makes a bare `larri up` a valid invocation
	// (FR-CRIT-04). Checking first would make the saved model unusable.
	if *model == "" {
		return errors.New("no model: pass --model, or save one with 'larri config'")
	}

	cfg := res.Config
	if *idleFor != 0 {
		cfg.Idle.Timeout = *idleFor
	}
	if *idleAct != "" {
		cfg.Idle.Action = config.IdleAction(*idleAct)
	}
	if set["budget"] {
		cfg.Budget.MaxUSD = *budget
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	mode := config.DetectMode(config.Invocation{ForceNonInteractive: *yes}, os.Getenv)
	firstRun(cfg, mode, res.File != "")

	prov, err := openProvider(*providerName)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	// Renting a second rig while a first one is still billing is the exact
	// leak this product exists to prevent, and the likeliest way to cause it
	// is an operator whose process died reaching for `up` again. `resume`
	// (FR-SUP-13) is what they usually want, so name it rather than making
	// them discover it after the invoice.
	//
	// This refuses rather than warns: a warning printed above a bring-up that
	// proceeds anyway is read after the money is spent.
	if !*dryRun {
		if err := refuseIfAlreadyBilling(st); err != nil {
			return err
		}
	}

	name := *served
	if name == "" {
		name = strings.ToLower(filepath.Base(*model))
	}
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
	provider.Report(p, func(e error) { fmt.Printf("  ! drift      %v\n", e) }, func(m string) { fmt.Printf("  ! search     %s\n", m) })

	// Sealing the provider-side marker is configuration, so an unset key is a
	// reported state rather than a silent one: the operator should know their
	// rig details are readable by the host, not assume otherwise.
	labelKey, keySrc, err := config.ResolveLabelKey(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	sealer, err := config.LabelSealer(labelKey)
	if err != nil {
		return err
	}
	fmt.Printf("  labels      %s — %s\n\n", keySrc, config.LabelKeyNotice(keySrc))

	spec := core.ModelSpec{
		Ref: *model, Source: core.SourceHuggingFace, ServedName: name,
		Quantization: *quant, ContextLen: *ctxLen,
	}
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
	for _, note := range securityNotes(eng) {
		fmt.Printf("  ! runtime    %s\n", note)
	}

	o := &daemon.Orchestrator{
		Store: st, Provider: p, Runtime: eng,
		LabelSealer: sealer,
		Resolver:    resolver,
		Policy: rank.Policy{
			ReliabilityFloor: *minRel,
			OutlierFactor:    rank.DefaultPolicy().OutlierFactor,
			MinClassSample:   rank.DefaultPolicy().MinClassSample,
		},
		Deadline: 30 * time.Minute,
		Events:   events,

		// The host enforces its own deadline, so an idle rig stops costing
		// money even if this process is killed. Derived from the local
		// timeout and always longer, so the supervisor — which can tell a
		// busy rig from an idle one — acts first.
		IdleTimeout:     cfg.Idle.Timeout,
		DeadmanDeadline: *deadman,
	}

	crit := core.Criteria{MaxPriceHr: *maxPrice, MinReliability: *minRel, DiskGB: *disk}
	if *gpu != "" {
		crit.GPUModel = splitList(*gpu)
	}
	req := daemon.UpRequest{
		Criteria:  crit,
		Model:     spec,
		DiskGB:    *disk,
		HFToken:   secret.New(os.Getenv("HF_TOKEN")),
		LocalPort: *port,
	}
	if *dryRun {
		req.Confirm = func(o core.Offer, p core.SizingPlan) bool {
			fmt.Printf("\n  dry run: would rent %s %s at $%.3f/hr — nothing spent\n",
				o.Provider, o.GPUModel, o.PriceHr)
			return false
		}
	} else if mode.Interactive() {
		req.Confirm = func(o core.Offer, _ core.SizingPlan) bool {
			fmt.Printf("\n  rent %s %dGB at $%.3f/hr? [y/N] ",
				o.GPUModel, o.VRAMTotalGB(), o.PriceHr)
			var in string
			fmt.Scanln(&in)
			return strings.EqualFold(strings.TrimSpace(in), "y")
		}
	}

	if *dryRun {
		_, err := o.Up(ctx, req)
		return err
	}

	live, err := o.UpAndServe(ctx, req)
	if err != nil {
		return err
	}
	rig := live.Rig
	fmt.Printf("\n  ✓ rig %s READY   %s   model: %s\n",
		rig.ID, live.Endpoint, rig.Model.ServedName)
	fmt.Printf("    %s %s at $%.3f/hr\n",
		rig.Offer.Provider, rig.Offer.GPUModel, rig.Offer.PriceHr)
	fmt.Printf("    key: %s\n", live.ClientToken.Reveal())
	fmt.Printf("\n  %s\n", daemon.PrivacyNotice(rig))

	// M1 has no daemon, so `up` holds the tunnel in the foreground. That is
	// honest rather than convenient: a tunnel is a live process, and exiting
	// while the rig bills would make `up` look successful and cost money.
	fmt.Printf("\n  %s\n", describePolicy(cfg))
	fmt.Printf("  holding the tunnel — Ctrl-C to tear down and stop paying\n\n")

	// The supervisor decides; it does not destroy. A nil return means the
	// operator interrupted, and an interrupt is not a reason to end a rig.
	term := o.Supervise(ctx, live, daemon.SupervisePolicy{
		Idle: cfg.Idle, Budget: cfg.Budget,
	})
	if term == nil {
		fmt.Printf("\n  interrupted; tearing down\n")
	} else {
		fmt.Printf("\n  ! %s — %s\n", term.Code, term.Summary)
	}

	live.Close()
	dctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := o.Down(dctx, rig, term); err != nil {
		return err
	}
	c := rig.End.Cost
	fmt.Printf("\n  ✓ rig %s DESTROYED  ran %s  total $%.4f\n",
		rig.ID, c.Ran.Round(time.Second), c.TotalUSD)
	return nil
}

func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	_ = fs.Parse(args)

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	prov, err := openProvider("")
	if err != nil {
		return err
	}
	rigs, err := st.List()
	if err != nil {
		return err
	}
	var target *core.Rig
	want := fs.Arg(0)
	for _, r := range rigs {
		if want != "" && r.ID != want {
			continue
		}
		if r.State.Billable() {
			target = r
			break
		}
	}
	if target == nil {
		fmt.Println("  nothing billable to tear down")
		return nil
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

	o := &daemon.Orchestrator{
		Store: st, Provider: prov,
		Runtime: vllm.New(), Events: events,
	}
	if err := o.Down(ctx, target, nil); err != nil {
		return err
	}
	c := target.End.Cost
	fmt.Printf("\n  ✓ rig %s DESTROYED  ran %s  total $%.4f\n",
		target.ID, c.Ran.Round(time.Second), c.TotalUSD)
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	all := fs.Bool("all", false, "include terminated rigs")
	_ = fs.Parse(args)

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	rigs, err := st.List()
	if err != nil {
		return err
	}
	entries, _ := st.Entries()
	now := time.Now().UTC()
	shown := 0
	for _, r := range rigs {
		if !*all && r.State == core.StateDestroyed {
			continue
		}
		shown++
		c := state.CostFor(entries, r.ID, now)
		fmt.Printf("  %s  %-13s $%.4f  ran %s\n",
			r.ID, r.State, c.TotalUSD, c.Ran.Round(time.Second))
		if r.Instance != nil {
			fmt.Printf("      %s instance %s  $%.3f/hr\n",
				r.Instance.Provider, r.Instance.InstanceID, r.Offer.PriceHr)
		}
		// §13.1: a rig that ended explains itself, long afterwards.
		if r.End != nil {
			fmt.Printf("      ended %s · %s: %s\n",
				r.End.At.Format(time.RFC3339), r.End.Actor, r.End.Summary)
			for k, v := range r.End.Evidence {
				fmt.Printf("        %s: %s\n", k, v)
			}
		}
		if r.State.Billable() {
			fmt.Printf("      %s\n", notice.PrivacyShort())
		}
	}
	if shown == 0 {
		fmt.Println("  no rigs")
	}
	return nil
}

// cmdResume reconnects to a rig that is still running at the provider after
// the process that created it went away.
//
// The tunnel it rebuilds was never a remote object: an SSH connection and a
// local listener, both of which died with that process. What is still there —
// and still billing — is the instance, with the model resident in VRAM. So
// this reconnects rather than relaunches, and the endpoint clients were wired
// to comes back at the same address.
func cmdResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	_ = fs.Parse(args)

	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	prov, err := openProvider("")
	if err != nil {
		return err
	}
	rigs, err := st.List()
	if err != nil {
		return err
	}
	var target *core.Rig
	want := fs.Arg(0)
	for _, r := range rigs {
		if want != "" && r.ID != want {
			continue
		}
		if r.State.Billable() {
			target = r
			break
		}
	}
	if target == nil {
		fmt.Println("  nothing billable to resume")
		return nil
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

	o := &daemon.Orchestrator{
		Store: st, Provider: prov,
		Runtime: vllm.New(), Events: events,
	}
	live, err := o.Adopt(ctx, target.ID)
	if err != nil {
		// A rig that cannot be reconnected to is still a rig that is billing.
		// Saying so here is the difference between an operator who destroys it
		// and one who assumes a failed command changed nothing.
		fmt.Fprintf(os.Stderr, "\n  ! rig %s is still billing at $%.3f/hr — 'larri down' destroys it\n",
			target.ID, target.Offer.PriceHr)
		return err
	}
	defer live.Close()

	fmt.Printf("\n  ✓ rig %s READY   %s   model: %s\n",
		target.ID, live.Endpoint, target.Model.ServedName)
	fmt.Printf("    $%.3f/hr · reconnected without restarting the model\n\n", target.Offer.PriceHr)
	fmt.Println(notice.PrivacyHeadline)

	<-ctx.Done()
	fmt.Println("\n  interrupted — the rig keeps running; 'larri down' destroys it")
	return nil
}

// refuseIfAlreadyBilling stops a second rig from being rented while a first
// one is unaccounted for.
//
// It reads local state rather than querying the provider, deliberately: a
// provider that is slow or unreachable must not become a reason to spend
// money, and the rig this catches is by definition one LARRI recorded.
func refuseIfAlreadyBilling(st *state.Store) error {
	rigs, err := st.List()
	if err != nil {
		return err
	}
	for _, r := range rigs {
		if !r.State.Billable() {
			continue
		}
		return fmt.Errorf("rig %s is already billing at $%.3f/hr (%s)\n"+
			"    reconnect to it   larri resume %s\n"+
			"    or destroy it     larri down %s",
			r.ID, r.Offer.PriceHr, r.State, r.ID, r.ID)
	}
	return nil
}

// describePolicy states the deadlines a rig is running under.
//
// FR-CFG-03 applied to the moment it matters: the operator is about to walk
// away from something that bills by the second, and a destructive default they
// were never told about is a trap however well reasoned.
func describePolicy(cfg config.Config) string {
	parts := make([]string, 0, 2)
	if cfg.Idle.Timeout > 0 {
		parts = append(parts, fmt.Sprintf("idle %s → %s", cfg.Idle.Timeout, cfg.Idle.Action))
	} else {
		parts = append(parts, "no idle timeout")
	}
	if cfg.Budget.MaxUSD > 0 {
		parts = append(parts, fmt.Sprintf("budget $%.2f → destroy", cfg.Budget.MaxUSD))
	} else {
		parts = append(parts, "no budget ceiling")
	}
	return "policy: " + strings.Join(parts, " · ")
}
