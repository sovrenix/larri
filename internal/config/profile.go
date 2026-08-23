// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"fmt"
	"strings"

	"go.sovrenix.com/larri/internal/core"
)

// DefaultProfile is the profile used when none is named.
const DefaultProfile = "default"

// Profile is a saved starting point for a rig: what to serve and what to serve
// it on.
//
// It carries its own YAML tags rather than reusing core.Criteria's, because a
// file format is a configuration concern and the domain model should not
// acquire one. It also holds the model fields, which are not criteria at all —
// an operator who has settled on a model and a quantisation should not have to
// retype them either.
//
// Every field is optional. A profile is an optimisation, never a requirement
// (FR-CFG-01), so an empty one behaves exactly as no profile at all.
type Profile struct {
	Model        string `yaml:"model,omitempty"`
	Quantization string `yaml:"quantization,omitempty"`
	ContextLen   int    `yaml:"context,omitempty"`
	ServedName   string `yaml:"served_name,omitempty"`
	Runtime      string `yaml:"runtime,omitempty"`

	GPUModel       []string `yaml:"gpu,omitempty"`
	VRAMPerGPUGB   int      `yaml:"vram_per_gpu_gb,omitempty"`
	DiskGB         int      `yaml:"disk_gb,omitempty"`
	Regions        []string `yaml:"regions,omitempty"`
	MinReliability float64  `yaml:"min_reliability,omitempty"`

	// MaxPriceHr is a spending limit, and a limit read from a file is
	// disclosed on every run that uses it (FR-CFG-08). A stale ceiling fails
	// as "no offer satisfies the criteria", which reads as a market problem
	// rather than a configuration one.
	MaxPriceHr float64 `yaml:"max_price_hr,omitempty"`

	LocalPort int `yaml:"local_port,omitempty"`
}

// Criteria renders the search half of a profile.
func (p Profile) Criteria() core.Criteria {
	return core.Criteria{
		GPUModel:       p.GPUModel,
		VRAMPerGPUGB:   p.VRAMPerGPUGB,
		DiskGB:         p.DiskGB,
		Regions:        p.Regions,
		MaxPriceHr:     p.MaxPriceHr,
		MinReliability: p.MinReliability,
	}
}

// Summary renders a profile for a human deciding whether it is the one they
// meant.
func (p Profile) Summary() string {
	var parts []string
	if p.Model != "" {
		m := p.Model
		if p.Quantization != "" {
			m += " @ " + p.Quantization
		}
		parts = append(parts, m)
	}
	if len(p.GPUModel) > 0 {
		parts = append(parts, strings.Join(p.GPUModel, "/"))
	}
	if p.MaxPriceHr > 0 {
		parts = append(parts, fmt.Sprintf("≤$%.3f/hr", p.MaxPriceHr))
	}
	if p.MinReliability > 0 {
		parts = append(parts, fmt.Sprintf("rel ≥%.2f", p.MinReliability))
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, " · ")
}

// Validate rejects a profile that cannot be acted on.
func (p Profile) Validate() error {
	switch {
	case p.MaxPriceHr < 0:
		return fmt.Errorf("config: max_price_hr must not be negative, got %.3f", p.MaxPriceHr)
	case p.MinReliability < 0 || p.MinReliability > 1:
		return fmt.Errorf("config: min_reliability must be between 0 and 1, got %.2f", p.MinReliability)
	case p.ContextLen < 0:
		return fmt.Errorf("config: context must not be negative, got %d", p.ContextLen)
	case p.DiskGB < 0:
		return fmt.Errorf("config: disk_gb must not be negative, got %d", p.DiskGB)
	case p.LocalPort < 0 || p.LocalPort > 65535:
		return fmt.Errorf("config: local_port out of range, got %d", p.LocalPort)
	}
	return nil
}
