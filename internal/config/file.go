// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Path is where the configuration file lives, honouring XDG.
func Path() string {
	if p := os.Getenv("LARRI_CONFIG"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "larri", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "larri", "config.yaml")
}

// Load reads the configuration file.
//
// A missing file is not an error: LARRI operates fully with no configuration
// (FR-CFG-01), so absence returns the built-in defaults and reports that no
// file was found. A file that exists and cannot be parsed *is* an error —
// silently falling back to defaults there would mean an operator's ceilings
// and policies vanish because of a stray tab.
func Load(path string) (Config, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return Default(), false, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Default(), false, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Default(), false, fmt.Errorf("config: %s: %w", path, err)
	}
	for name, p := range cfg.Profiles {
		if err := p.Validate(); err != nil {
			return Default(), false, fmt.Errorf("config: %s: profile %q: %w", path, name, err)
		}
	}
	return cfg, true, nil
}

// Save writes the configuration file, creating its directory.
//
// Written through a temp file and renamed, for the same reason the state store
// is: a crash mid-write must leave the previous complete file rather than a
// truncated one that fails to parse and takes the operator's ceilings with it.
func Save(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	for name, p := range cfg.Profiles {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("config: profile %q: %w", name, err)
		}
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: encode: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}
	out := header() + string(body)

	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("config: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	// 0600: the file carries no secrets by design (FR-SEC-01), but it does
	// carry spending limits, and those are nobody else's business either.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("config: permissions on %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("config: install %s: %w", path, err)
	}
	return nil
}

// header is the comment block at the top of a written file.
//
// The privacy notice goes in the file itself because the file outlives the
// terminal that created it: someone reading this months later, or on another
// machine, should meet the same warning the first run gave (§15.7). This is
// the reason the format has comments at all.
func header() string {
	var b strings.Builder
	b.WriteString("# LARRI configuration — https://go.sovrenix.com/larri\n#\n")
	b.WriteString("# Values resolve: flags → profile → this file → built-in defaults.\n")
	b.WriteString("# Every layer is optional; LARRI runs with no configuration at all.\n#\n")
	b.WriteString("# WARNING: rigs bill by the second. idle.action and budget.action default\n")
	b.WriteString("# to `destroy` deliberately — a rig that outlives your attention is the\n")
	b.WriteString("# failure this program exists to prevent.\n#\n")
	b.WriteString("# Prompts sent to a rig travel to rented third-party hardware whose\n")
	b.WriteString("# operator has root on it. Send nothing you could not afford to disclose.\n#\n")
	b.WriteString("# ssh_timeout controls how long a new host may take to accept the rig key.\n")
	b.WriteString("# Secrets are never stored here. API keys come from the environment or the\n")
	b.WriteString("# OS keyring (FR-SEC-01).\n\n")
	return b.String()
}

// EnsureExists writes a defaults file if none is present.
//
// It creates and returns; it never prompts. Configuration creation belongs in
// core rather than in a surface (FR-CFG-04), and a surface that blocked on a
// form here would hang every non-interactive caller — the MCP server most of
// all, where a terminal prompt produces a protocol stream that never speaks
// again (FR-CFG-02).
//
// Reports whether it wrote, so the caller can say so. A configuration file
// that appeared without being mentioned is the same trap as an undisclosed
// destructive default.
func EnsureExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("config: check %s: %w", path, err)
	}
	cfg := Default()
	cfg.Profiles = map[string]Profile{DefaultProfile: {}}
	if err := Save(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}
