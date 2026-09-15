// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// The shipped example is the first thing an operator runs. A broken one is a
// broken first impression, and it is free to check.
func TestShippedExampleParses(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "comfy", "sdxl.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("example not readable from here: %v", err)
	}
	g, err := Parse(b)
	if err != nil {
		t.Fatalf("the shipped example does not parse: %v", err)
	}
	if !g.Format.Executable() {
		t.Error("the shipped example is not in the api serialisation, so /prompt would reject it")
	}
	if len(g.Assets) == 0 {
		t.Fatal("the shipped example names no models")
	}
	if g.Image.Width == 0 || g.Steps == 0 {
		t.Errorf("the example states no latent or no steps: %+v steps=%d", g.Image, g.Steps)
	}
	// It must also resolve with no manifest at all, which is the claim the
	// built-in catalogue makes for the stock filenames.
	for _, name := range g.Names() {
		if _, ok := (*Manifest)(nil).Lookup(name); !ok {
			t.Errorf("the example names %q, which the catalogue cannot resolve "+
				"and no manifest is shipped for", name)
		}
	}
}

// The shipped manifest must load, or the flag that points at it fails on the
// one file we provide.
func TestShippedManifestLoads(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "comfy", "models.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example not readable from here: %v", err)
	}
	if _, err := LoadManifest(path); err != nil {
		t.Fatalf("the shipped manifest does not load: %v", err)
	}
}
