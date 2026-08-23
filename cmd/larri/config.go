// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/provider/vastai"
	"go.sovrenix.com/larri/internal/rank"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/tui"
)

// cmdConfig edits a criteria profile, previewing what it would rent.
//
// Editing lives in its own command rather than inside `larri up`, because `up`
// is a command that spends: opening a form in front of an operator who asked
// to rent hardware is a surprise, and a Ctrl-C halfway through leaves them
// asking what happened. FR-CFG-01 says configuration is an optimisation, never
// a prerequisite, and a wizard on the spending path contradicts that.
func cmdConfig(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	profile := fs.String("profile", "", "edit one profile directly (default: list them all)")
	create := fs.Bool("new", false, "create the named profile if it does not exist")
	show := fs.Bool("show", false, "print the resolved configuration and exit")
	path := fs.String("path", "", "configuration file (default: XDG location)")
	_ = fs.Parse(args)

	file := *path
	if file == "" {
		file = config.Path()
	}
	created, err := config.EnsureExists(file)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("  created     %s\n\n", file)
	}
	cfg, _, err := config.Load(file)
	if err != nil {
		return err
	}

	if *show {
		return showConfig(file, cfg)
	}

	if cfg.Profiles == nil {
		cfg.Profiles = map[string]config.Profile{}
	}
	// Checked before the terminal is, because a name that does not exist is a
	// usage error either way — and finding out about a typo should not
	// require a tty to find out in.
	if *profile != "" {
		if _, ok := cfg.Profiles[*profile]; !ok && !*create {
			return fmt.Errorf("no profile %q in %s: pass --new to create it, "+
				"or run 'larri config' to list what is there", *profile, file)
		}
	}

	mode := config.DetectMode(config.Invocation{}, os.Getenv)
	if !mode.Interactive() {
		// FR-CFG-02. A form needs a terminal, and a surface that opened one
		// anyway would hang whatever launched it with no output and no error.
		return errors.New("editing needs a terminal; use --show, or edit " + file)
	}
	save := func(set map[string]config.Profile) error {
		for name, p := range set {
			if err := p.Validate(); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		cfg.Profiles = set
		return config.Save(file, cfg)
	}

	if *profile != "" {
		return editOne(ctx, file, cfg, *profile, *create, save)
	}
	return pickAndEdit(ctx, file, cfg, save)
}

// editOne opens the editor on a named profile.
//
// A name that does not exist is refused unless --new. `larri up --profile
// codr` already errors on an unknown name; a `config` that silently created
// one instead would let a typo produce a phantom profile that only reveals
// itself as "no profile" from the other command.
func editOne(ctx context.Context, file string, cfg config.Config, name string,
	create bool, save func(map[string]config.Profile) error) error {

	if _, ok := cfg.Profiles[name]; !ok {
		if !create {
			return fmt.Errorf("no profile %q in %s: pass --new to create it, "+
				"or run 'larri config' to list what is there", name, file)
		}
		cfg.Profiles[name] = config.Profile{}
	}
	ed := tui.NewEditor(name, cfg.Profiles[name])
	ed.Preview = previewFunc(ctx)
	ed.Save = func(p config.Profile) error {
		set := cfg.Profiles
		set[name] = p
		return save(set)
	}
	final, err := tea.NewProgram(ed, tea.WithContext(ctx)).Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	if m, ok := final.(tui.Editor); ok {
		if _, saved := m.Done(); saved {
			fmt.Printf("\n  ✓ saved profile %q to %s\n", name, file)
			fmt.Printf("    %s\n", m.Result().Summary())
			fmt.Printf("\n  use it with: larri up --profile %s\n", name)
			return nil
		}
	}
	fmt.Println("\n  nothing saved")
	return nil
}

// pickAndEdit lists the profiles and edits whichever is chosen.
func pickAndEdit(ctx context.Context, file string, cfg config.Config,
	save func(map[string]config.Profile) error) error {

	pk := tui.NewProfiles(cfg.Profiles)
	pk.Preview = previewFunc(ctx)
	pk.Save = save

	final, err := tea.NewProgram(pk, tea.WithContext(ctx)).Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	m, ok := final.(tui.Profiles)
	if !ok {
		return nil
	}
	set := m.Result()
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("\n  %s\n", file)
	for _, n := range names {
		fmt.Printf("    %-14s %s\n", n, set[n].Summary())
	}
	if len(names) == 0 {
		fmt.Println("    (no profiles)")
	}
	return nil
}

// previewFunc ranks the market for whatever the editor currently holds.
//
// It spends nothing — the same survey `larri offers` runs — which is what
// makes editing here worth doing: the answer to "is that ceiling too low?"
// arrives in a second rather than after a failed rental.
func previewFunc(ctx context.Context) func(config.Profile) tea.Cmd {
	return func(p config.Profile) tea.Cmd {
		return func() tea.Msg {
			key := os.Getenv("VASTAI_API_KEY")
			if key == "" {
				return tui.PreviewMsg{Err: errors.New("VASTAI_API_KEY is not set, so offers cannot be previewed")}
			}
			spec := core.ModelSpec{
				Ref: p.Model, Source: core.SourceHuggingFace, ServedName: "preview",
				Quantization: p.Quantization, ContextLen: p.ContextLen,
			}
			if isOllamaRef(p.Model) {
				spec.Source = core.SourceOllamaRegistry
			}
			pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			resolver, err := prepareSpec(pctx, &spec)
			if err != nil {
				return tui.PreviewMsg{Err: err}
			}
			eng, err := pickRuntime(p.Runtime, spec)
			if err != nil {
				return tui.PreviewMsg{Err: err}
			}
			o := &daemon.Orchestrator{
				Provider: vastai.New(secret.New(key)), Runtime: eng,
				Resolver: resolver, Policy: rank.DefaultPolicy(),
			}
			crit := p.Criteria()
			if crit.DiskGB == 0 {
				crit.DiskGB = 60
			}
			sv, err := o.Offers(pctx, daemon.UpRequest{
				Criteria: crit, Model: spec, DiskGB: crit.DiskGB,
			})
			if err != nil {
				return tui.PreviewMsg{Err: err}
			}
			var rows []tui.PreviewRow
			for _, c := range eligibleTop(sv.Selection, 5) {
				rows = append(rows, tui.PreviewRow{
					GPU: c.Offer.GPUModel, VRAMGB: c.Offer.VRAMTotalGB(),
					PriceHr: c.Offer.PriceHr, Reliability: c.Offer.Reliability,
					Selected: sv.Selection.Selected != nil &&
						c.Offer.OfferID == sv.Selection.Selected.Offer.OfferID,
				})
			}
			return tui.PreviewMsg{Rows: rows}
		}
	}
}

// showConfig prints the resolved configuration without a terminal.
func showConfig(file string, cfg config.Config) error {
	fmt.Printf("  file        %s\n", file)
	fmt.Printf("  idle        %s then %s\n", config.Duration(cfg.Idle.Timeout), cfg.Idle.Action)
	if cfg.Budget.MaxUSD > 0 {
		fmt.Printf("  budget      $%.2f then %s\n", cfg.Budget.MaxUSD, cfg.Budget.Action)
	} else {
		fmt.Printf("  budget      none\n")
	}
	fmt.Printf("  local port  %d\n", cfg.LocalPort)
	if len(cfg.Profiles) == 0 {
		fmt.Println("  profiles    none")
		return nil
	}
	// Sorted, because Go randomises map iteration and a listing that
	// reordered itself between runs is one an operator cannot scan.
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Println("  profiles")
	for _, name := range names {
		marker := ""
		if name == config.DefaultProfile {
			marker = "  ← used by a bare `larri up`"
		}
		fmt.Printf("    %-14s %s%s\n", name, cfg.Profiles[name].Summary(), marker)
	}
	return nil
}

// applyProfile fills flags the operator did not pass from a saved profile.
//
// Flags win, always — a profile is a starting point, not an authority
// (FR-CFG-01). Which is why this needs to know what was *passed* rather than
// what a flag currently holds: a flag left at its zero value and a flag
// explicitly set to zero mean opposite things, and only the second should
// override a saved ceiling.
func applyProfile(p config.Profile, set map[string]bool,
	model, quant *string, ctxLen *int, gpu *string, maxPrice *float64,
	disk *int, minRel *float64, port *int, engine *string) {

	if !set["model"] && p.Model != "" {
		*model = p.Model
	}
	if !set["quantization"] && p.Quantization != "" {
		*quant = p.Quantization
	}
	if !set["context"] && p.ContextLen > 0 {
		*ctxLen = p.ContextLen
	}
	if !set["gpu"] && len(p.GPUModel) > 0 {
		*gpu = strings.Join(p.GPUModel, ",")
	}
	if !set["max-price"] && p.MaxPriceHr > 0 {
		*maxPrice = p.MaxPriceHr
	}
	if !set["disk"] && p.DiskGB > 0 {
		*disk = p.DiskGB
	}
	if !set["min-reliability"] && p.MinReliability > 0 {
		*minRel = p.MinReliability
	}
	if !set["port"] && p.LocalPort > 0 {
		*port = p.LocalPort
	}
	if !set["runtime"] && p.Runtime != "" {
		*engine = p.Runtime
	}
}

// splitList parses a comma-separated flag value.
//
// Shared with the editor's parsing so a `--gpu` typed on the command line and
// one saved in a profile mean the same thing.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
