// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"fmt"
	"sort"
	"strings"
)

// Factory builds a provider from the environment.
//
// Credentials resolve inside the factory rather than being passed in, because
// each provider names its own key and the caller should not have to know which
// (FR-SEC-01). A factory that cannot find its credential returns an error
// naming the variable, which is the one thing the operator needs to hear.
type Factory func() (Provider, error)

var registry = map[string]Factory{}

// Register adds a provider under its name.
//
// Called from adapter package init. Duplicate registration panics rather than
// overwriting: two adapters answering to one name would make which is used
// depend on import order, and the wrong one spends money.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("provider: " + name + " registered twice")
	}
	registry[name] = f
}

// Open builds a provider by name.
func Open(name string) (Provider, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q: known are %s", name, strings.Join(Names(), ", "))
	}
	return f()
}

// Names lists the registered providers.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Default returns the provider to use when the operator names none.
//
// With one registered it is that one; with several it is an error rather than
// a guess. Choosing between providers on the operator's behalf means choosing
// which account gets billed, and there is no defensible default for that.
func Default() (string, error) {
	names := Names()
	switch len(names) {
	case 0:
		return "", fmt.Errorf("no providers are compiled in")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("several providers available (%s): name one with --provider",
			strings.Join(names, ", "))
	}
}
