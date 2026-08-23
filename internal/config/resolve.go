// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"fmt"
	"time"
)

// Sourced is one setting that came from somewhere other than a flag, and that
// the operator needs told about anyway.
type Sourced struct {
	Setting string
	Value   string
	From    string // "config" or "profile <name>"
}

// Resolution is the outcome of layering flags over a profile over a file over
// the built-in defaults (FR-CFG-01).
type Resolution struct {
	Config  Config
	Profile Profile
	Name    string // profile applied, empty if none
	File    string // path read, empty if none existed
	Created bool   // a defaults file was written on this run

	// Disclose lists the money and destructive settings that came from the
	// file rather than from a flag.
	//
	// FR-CFG-03 requires disclosure when configuration is *created*. This is
	// the same argument applied to every run that *uses* it, and it exists
	// because both directions of a stale limit are bad: a low ceiling fails
	// as "no offer satisfies the criteria", which reads as a market problem,
	// and a high one silently removes a guard the operator believes is on.
	Disclose []Sourced
}

// Request describes what a surface is asking for.
type Request struct {
	// Path to the configuration file. Empty means Path().
	Path string

	// Profile names the profile to apply. Empty means the default one, and a
	// missing default is not an error — it simply means no profile layer.
	Profile string

	// SetFlags names the flags the operator passed explicitly, so that a
	// flag set to a value equal to the default still wins over the file.
	// Without this, `--max-price 0` could not turn a saved ceiling off.
	SetFlags map[string]bool

	// Ensure writes a defaults file when none exists.
	Ensure bool
}

// Resolve layers configuration and reports what came from where.
func Resolve(req Request) (*Resolution, error) {
	path := req.Path
	if path == "" {
		path = Path()
	}
	res := &Resolution{}

	if req.Ensure {
		created, err := EnsureExists(path)
		if err != nil {
			return nil, err
		}
		res.Created = created
	}

	cfg, found, err := Load(path)
	if err != nil {
		return nil, err
	}
	res.Config = cfg
	if found {
		res.File = path
	}

	name := req.Profile
	explicit := name != ""
	if name == "" {
		name = DefaultProfile
	}
	prof, ok := cfg.Profiles[name]
	switch {
	case ok:
		res.Profile, res.Name = prof, name
	case explicit:
		// An operator who named a profile and got silence would be running
		// against defaults while believing otherwise.
		return nil, fmt.Errorf("config: no profile %q in %s", name, path)
	}

	set := req.SetFlags
	if set == nil {
		set = map[string]bool{}
	}
	from := "config"
	if res.Name != "" {
		from = "profile " + res.Name
	}
	if res.File != "" {
		if res.Profile.MaxPriceHr > 0 && !set["max-price"] {
			res.Disclose = append(res.Disclose, Sourced{
				Setting: "max-price", From: from,
				Value: fmt.Sprintf("$%.3f/hr", res.Profile.MaxPriceHr),
			})
		}
		if cfg.Budget.MaxUSD > 0 && !set["budget"] {
			res.Disclose = append(res.Disclose, Sourced{
				Setting: "budget", From: "config",
				Value: fmt.Sprintf("$%.2f then %s", cfg.Budget.MaxUSD, cfg.Budget.Action),
			})
		}
		if !set["idle-timeout"] && cfg.Idle.Timeout != Default().Idle.Timeout {
			res.Disclose = append(res.Disclose, Sourced{
				Setting: "idle-timeout", From: "config",
				Value: fmt.Sprintf("%s then %s", shortDur(cfg.Idle.Timeout), cfg.Idle.Action),
			})
		}
		if !set["idle-action"] && cfg.Idle.Action != Default().Idle.Action {
			res.Disclose = append(res.Disclose, Sourced{
				Setting: "idle-action", From: "config", Value: string(cfg.Idle.Action),
			})
		}
	}
	return res, nil
}

// Duration renders a timeout the way the disclosure lines do.
func Duration(d time.Duration) string { return shortDur(d) }
