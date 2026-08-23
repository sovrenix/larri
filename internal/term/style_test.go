// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package term

import (
	"os"
	"strings"
	"testing"
)

func TestStyleWrapsAndAlwaysResets(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	// t.Setenv with an empty value still sets it, so clear it properly.
	if err := unsetenv(t, "NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	got := NewStyle().Foreground("214").Bold(true).Render("hi")
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Errorf("no reset; the style would leak into the next output: %q", got)
	}
	if !strings.Contains(got, "38;5;214") || !strings.Contains(got, "1;") {
		t.Errorf("attributes missing: %q", got)
	}
	if !strings.Contains(got, "hi") {
		t.Errorf("text lost: %q", got)
	}
}

// https://no-color.org — any value, including empty, disables colour. Piping
// output into a log should not fill it with escape sequences.
func TestNoColorIsHonoured(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if got := NewStyle().Foreground("214").Bold(true).Render("hi"); got != "hi" {
		t.Errorf("NO_COLOR ignored: %q", got)
	}
}

func TestDumbTerminalGetsNoEscapes(t *testing.T) {
	if err := unsetenv(t, "NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "dumb")
	if got := NewStyle().Foreground("214").Render("hi"); got != "hi" {
		t.Errorf("dumb terminal got escapes: %q", got)
	}
}

// An unstyled Style must add nothing at all, so plain text stays plain.
func TestUnstyledIsPassThrough(t *testing.T) {
	if got := NewStyle().Render("plain"); got != "plain" {
		t.Errorf("got %q", got)
	}
}

func TestAdaptivePicksFromCOLORFGBG(t *testing.T) {
	for _, c := range []struct {
		env  string
		want Color
	}{
		{"15;0", "dark"},  // light text on dark background
		{"0;15", "light"}, // dark text on light background
		{"0;7", "light"},  // the other light grey
		{"", "dark"},      // unset: assume dark, the common case
		{"nonsense", "dark"},
	} {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv("COLORFGBG", c.env)
			if got := Adaptive("light", "dark"); got != c.want {
				t.Errorf("COLORFGBG=%q → %q, want %q", c.env, got, c.want)
			}
		})
	}
}

// unsetenv removes a variable for the duration of a test. t.Setenv cannot:
// setting it to "" still counts as set, which is exactly what NO_COLOR treats
// as "disable colour".
func unsetenv(t *testing.T, k string) error {
	t.Helper()
	old, had := lookupEnv(k)
	if err := osUnsetenv(k); err != nil {
		return err
	}
	t.Cleanup(func() {
		if had {
			_ = osSetenv(k, old)
		} else {
			_ = osUnsetenv(k)
		}
	})
	return nil
}

var (
	lookupEnv  = os.LookupEnv
	osUnsetenv = os.Unsetenv
	osSetenv   = os.Setenv
)
