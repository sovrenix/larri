// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package config resolves LARRI's non-secret settings.
//
// The load-bearing property is that **there is nothing to configure before
// LARRI works**. FR-CRIT-04 requires `larri up --model <ref>` to be valid on
// its own, so configuration is an optimisation over defaults and never a
// prerequisite. Values resolve flags → profile → file → defaults, and every
// layer is optional (FR-CFG-01).
//
// Secrets are deliberately absent from this type. They resolve separately from
// the environment or the OS keyring and are never written to a config file
// (FR-SEC-01).
package config

import (
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// IdleAction is what happens when a rig goes unused.
type IdleAction string

const (
	// IdleDestroy is the default. Forgetting is the failure this product
	// exists to prevent, and a warning nobody reads prevents nothing.
	IdleDestroy IdleAction = "destroy"
	IdleWarn    IdleAction = "warn"
)

// Destructive reports whether the action ends a rig. Used to decide what must
// be disclosed on first run (FR-CFG-03).
func (a IdleAction) Destructive() bool { return a == IdleDestroy }

// Idle is the idle-reclamation policy.
type Idle struct {
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
	Action  IdleAction    `yaml:"action" json:"action"`
}

// Budget is the spend ceiling policy. A zero Max means no ceiling.
type Budget struct {
	MaxUSD float64    `yaml:"max_usd" json:"max_usd"`
	Action IdleAction `yaml:"action" json:"action"`
}

// Config is the resolved, non-secret settings.
type Config struct {
	Providers         []string                 `yaml:"providers" json:"providers"`
	LocalPort         int                      `yaml:"local_port" json:"local_port"`
	MaxConcurrentRigs int                      `yaml:"max_concurrent_rigs" json:"max_concurrent_rigs"`
	Idle              Idle                     `yaml:"idle" json:"idle"`
	Budget            Budget                   `yaml:"budget" json:"budget"`
	Clients           []string                 `yaml:"clients" json:"clients"`
	RankWeights       RankWeights              `yaml:"rank_weights" json:"rank_weights"`
	Profiles          map[string]core.Criteria `yaml:"profiles,omitempty" json:"profiles,omitempty"`
}

// RankWeights are the scoring weights of DES §8.
type RankWeights struct {
	Price       float64 `yaml:"price" json:"price"`
	Fit         float64 `yaml:"fit" json:"fit"`
	Reliability float64 `yaml:"reliability" json:"reliability"`
	Net         float64 `yaml:"net" json:"net"`
	Region      float64 `yaml:"region" json:"region"`
}

// Default returns the built-in configuration.
//
// These values are what runs on a machine that has never been configured, so
// they are chosen to be safe rather than permissive: interruptible offers are
// excluded (Q-04), one rig at a time (Q-05), and both reclamation policies
// destroy (Q-11, Q-03) rather than warn.
func Default() Config {
	return Config{
		Providers:         []string{"vastai"},
		LocalPort:         8000,
		MaxConcurrentRigs: 1,
		Idle:              Idle{Timeout: 30 * time.Minute, Action: IdleDestroy},
		Budget:            Budget{MaxUSD: 0, Action: IdleDestroy},
		Clients:           nil, // populated by detection, never guessed
		RankWeights: RankWeights{
			Price: 0.40, Fit: 0.20, Reliability: 0.20, Net: 0.10, Region: 0.10,
		},
	}
}

// Validate reports configuration that cannot be honoured.
func (c Config) Validate() error {
	if c.LocalPort < 1 || c.LocalPort > 65535 {
		return fmt.Errorf("config: local_port %d out of range: expected 1-65535", c.LocalPort)
	}
	if c.MaxConcurrentRigs < 1 {
		return fmt.Errorf("config: max_concurrent_rigs must be at least 1, got %d",
			c.MaxConcurrentRigs)
	}
	if c.Idle.Action != IdleDestroy && c.Idle.Action != IdleWarn {
		return fmt.Errorf("config: idle.action must be destroy or warn, got %q", c.Idle.Action)
	}
	if c.Budget.MaxUSD < 0 {
		return fmt.Errorf("config: budget.max_usd must not be negative, got %.2f", c.Budget.MaxUSD)
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("config: at least one provider must be enabled")
	}
	return nil
}

// Disclosure is a default that the operator has not chosen and that LARRI must
// state anyway.
type Disclosure struct {
	Setting     string // "idle-timeout"
	Value       string // "30m → destroy"
	Destructive bool
	ChangeWith  string // the command that changes it
}

// Disclosures returns the settings that must be printed on any run that
// creates a configuration, interactive or not (FR-CFG-03).
//
// Idle reclamation and budget breach both default to destroying a rig. Those
// defaults are correct, but a default that destroys and was never mentioned is
// a trap however well it is reasoned — so the disclosure is not conditional on
// there being a human to read it.
func (c Config) Disclosures() []Disclosure {
	out := []Disclosure{{
		Setting:     "idle-timeout",
		Value:       fmt.Sprintf("%s → %s", shortDur(c.Idle.Timeout), c.Idle.Action),
		Destructive: c.Idle.Action.Destructive(),
		ChangeWith:  "larri config set idle.action warn",
	}}
	if c.Budget.MaxUSD == 0 {
		out = append(out, Disclosure{
			Setting:    "budget",
			Value:      "none set",
			ChangeWith: "larri config set budget.max_usd 5.00",
		})
	} else {
		out = append(out, Disclosure{
			Setting:     "budget",
			Value:       fmt.Sprintf("$%.2f → %s", c.Budget.MaxUSD, c.Budget.Action),
			Destructive: c.Budget.Action.Destructive(),
			ChangeWith:  "larri config set budget.action warn",
		})
	}
	return out
}

func shortDur(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}
