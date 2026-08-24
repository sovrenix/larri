// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package provider

import (
	"errors"
	"strings"
	"testing"
)

func withRegistry(t *testing.T, entries map[string]Factory) {
	t.Helper()
	old := registry
	registry = entries
	t.Cleanup(func() { registry = old })
}

// With one provider, no choice needs making. With several, choosing on the
// operator's behalf means choosing whose account gets billed — and there is no
// defensible default for that.
func TestDefaultRefusesToChooseBetweenProviders(t *testing.T) {
	withRegistry(t, map[string]Factory{"only": nil})
	if n, err := Default(); err != nil || n != "only" {
		t.Errorf("Default() = %q, %v; a single provider needs no choosing", n, err)
	}

	withRegistry(t, map[string]Factory{"a": nil, "b": nil})
	n, err := Default()
	if err == nil {
		t.Fatalf("Default() picked %q with two available", n)
	}
	if !strings.Contains(err.Error(), "--provider") {
		t.Errorf("the error should say how to choose: %v", err)
	}
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q as an option: %v", want, err)
		}
	}
}

func TestOpenNamesTheAlternativesWhenAskedForSomethingUnknown(t *testing.T) {
	withRegistry(t, map[string]Factory{"vastai": nil, "runpod": nil})
	_, err := Open("runpid")
	if err == nil {
		t.Fatal("opened a provider that does not exist")
	}
	for _, want := range []string{"runpid", "runpod", "vastai"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A factory reports a missing credential by naming the variable, which is the
// one thing the operator needs to hear.
func TestOpenSurfacesTheFactoryError(t *testing.T) {
	withRegistry(t, map[string]Factory{
		"p": func() (Provider, error) { return nil, errors.New("SOME_API_KEY is not set") },
	})
	_, err := Open("p")
	if err == nil || !strings.Contains(err.Error(), "SOME_API_KEY") {
		t.Errorf("err = %v; the missing variable must be named", err)
	}
}

// Two adapters answering to one name would make which is used depend on import
// order, and the wrong one spends money.
func TestDuplicateRegistrationPanics(t *testing.T) {
	withRegistry(t, map[string]Factory{})
	Register("dup", func() (Provider, error) { return nil, nil })
	defer func() {
		if recover() == nil {
			t.Error("registering a name twice was allowed")
		}
	}()
	Register("dup", func() (Provider, error) { return nil, nil })
}
