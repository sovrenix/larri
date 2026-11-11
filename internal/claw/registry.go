// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package claw

import (
	"fmt"
	"sort"
	"strings"
)

// Type names an application.
type Type string

// Factory builds a fresh Kind.
//
// A factory rather than a registered singleton, and the reason is not symmetry
// with the provider registry — it is that a Kind carries state between its own
// methods. Plan reads a config and resolves what it found; Server and Collect
// use it. A shared instance would make two concurrent claws overwrite each
// other's plan, and the failure would be a rig serving the wrong thing rather
// than an error.
type Factory func() Kind

var registry = map[Type]Factory{}

// Register adds a Kind under its type.
//
// Called from the implementation package's init, and blank-imported by the
// binary, exactly as the provider adapters are. Duplicate registration panics
// rather than overwriting: two Kinds answering to one name would make which is
// used depend on import order, and the wrong one spends money.
func Register(t Type, f Factory) {
	if t == "" {
		panic("claw: registered with an empty type")
	}
	if _, dup := registry[t]; dup {
		panic("claw: " + string(t) + " registered twice")
	}
	registry[t] = f
}

// Open returns a fresh Kind for a type.
func Open(t Type) (Kind, error) {
	f, ok := registry[t]
	if !ok {
		known := strings.Join(Types(), ", ")
		if known == "" {
			return nil, fmt.Errorf("unknown claw type %q: none are compiled in", t)
		}
		return nil, fmt.Errorf("unknown claw type %q: known are %s", t, known)
	}
	k := f()
	if k == nil {
		return nil, fmt.Errorf("claw %q built nothing", t)
	}
	if got := k.Type(); got != t {
		// A Kind registered under one name and answering to another would
		// make --type mean one thing to the registry and another to
		// everything downstream.
		return nil, fmt.Errorf("claw %q reports type %q", t, got)
	}
	if !k.Site().Valid() {
		return nil, fmt.Errorf("claw %q reports no valid site", t)
	}
	return k, nil
}

// Types lists the registered claw types, in a stable order.
func Types() []string {
	out := make([]string, 0, len(registry))
	for t := range registry {
		out = append(out, string(t))
	}
	sort.Strings(out)
	return out
}

// Describe lists the registered types with their site and description, for
// `larri claw --list`.
//
// The site is shown because it is the fact that changes what an operator gets:
// a remote claw is opened in a browser and its results are collected at
// teardown, a local one wires an application they already run.
func Describe() []string {
	var out []string
	for _, name := range Types() {
		k := registry[Type(name)]()
		out = append(out, fmt.Sprintf("%-12s %-6s %s", name, k.Site(), k.Describe()))
	}
	return out
}

// Registered reports whether a type is compiled in.
func Registered(t Type) bool {
	_, ok := registry[t]
	return ok
}
