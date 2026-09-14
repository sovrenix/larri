// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/clientkeys"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/secret"
)

// What a serving rig tells the operator about keys, in each case. The value
// of a stored key is shown exactly once — when it is made — and a key for one
// rig is shown when that rig is ready.
func TestTheReadyLineShowsAKeyOnlyWhenItIsNew(t *testing.T) {
	keys := clientkeys.Open(t.TempDir())
	stored := &daemon.Live{}

	// First run, nothing stored: a default key is made and shown, once.
	first, err := keyLine(stored, keys)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := keys.List()
	if len(list) != 1 || list[0].Name != "default" {
		t.Fatalf("keys after a first run: %+v", list)
	}
	value := strings.Fields(first)[0]
	if _, ok := keys.Match(value); !ok || !strings.Contains(first, "shown once") {
		t.Errorf("first ready line %q does not show the new default key", first)
	}

	// Every later rig names the keys and shows no value.
	later, err := keyLine(stored, keys)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(later, value) || !strings.Contains(later, "default") {
		t.Errorf("later ready line %q; want the key named, never shown again", later)
	}
	if list, _ := keys.List(); len(list) != 1 {
		t.Errorf("a second rig made another key: %+v", list)
	}

	// A key for this rig alone is shown, and no stored key is made for it.
	oneRig := &daemon.Live{ClientToken: secret.New("ONE-RIG-KEY")}
	line, err := keyLine(oneRig, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "ONE-RIG-KEY") || !strings.Contains(line, "this rig only") {
		t.Errorf("one-rig ready line %q", line)
	}
}
