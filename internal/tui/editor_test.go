// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"go.sovrenix.com/larri/internal/config"
)

func sendE(e Editor, msgs ...tea.Msg) Editor {
	for _, m := range msgs {
		next, _ := e.Update(m)
		e = next.(Editor)
	}
	return e
}

func typeIn(s string) []tea.Msg {
	out := []tea.Msg{tea.KeyMsg{Type: tea.KeyEnter}} // begin editing
	for _, r := range s {
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return append(out, tea.KeyMsg{Type: tea.KeyEnter}) // accept
}

func down(n int) []tea.Msg {
	var out []tea.Msg
	for i := 0; i < n; i++ {
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	return out
}

func TestEditingAFieldUpdatesTheProfile(t *testing.T) {
	e := sendE(NewEditor("default", config.Profile{}), typeIn("org/model")...)
	if e.Result().Model != "org/model" {
		t.Errorf("model = %q", e.Result().Model)
	}
}

// Escaping an edit must leave the value alone. A form that committed on the
// way out would change spending limits by accident.
func TestEscapeAbandonsAnEdit(t *testing.T) {
	e := NewEditor("default", config.Profile{Model: "original"})
	e = sendE(e, tea.KeyMsg{Type: tea.KeyEnter})
	e = sendE(e, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	e = sendE(e, tea.KeyMsg{Type: tea.KeyEsc})
	if e.Result().Model != "original" {
		t.Errorf("model = %q; an abandoned edit was committed", e.Result().Model)
	}
}

// A rejected value must not be written, and the operator must be told why.
// Silently keeping the old number would look like the keystroke was lost.
func TestInvalidInputIsRefusedWithAReason(t *testing.T) {
	e := NewEditor("default", config.Profile{MinReliability: 0.9})
	// min reliability is the sixth field
	e = sendE(e, down(5)...)
	e = sendE(e, typeIn("4.2")...)

	if e.Result().MinReliability != 0.9 {
		t.Errorf("reliability = %v; an impossible value was stored", e.Result().MinReliability)
	}
	if !strings.Contains(e.View(), "between 0 and 1") {
		t.Error("the refusal gave no reason")
	}
}

// Saving is the caller's job: writing configuration belongs in core, and a
// surface that owned it would be the second place the rules live.
func TestSaveDelegatesToTheCaller(t *testing.T) {
	var got config.Profile
	var called bool
	e := NewEditor("default", config.Profile{Model: "org/m"})
	e.Save = func(p config.Profile) error { got, called = p, true; return nil }

	e = sendE(e, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	if !called {
		t.Fatal("save did not reach the caller")
	}
	if got.Model != "org/m" {
		t.Errorf("saved %+v", got)
	}
	if done, saved := e.Done(); !done || !saved {
		t.Errorf("done=%v saved=%v", done, saved)
	}
}

// Quitting must not write. An operator who backs out has not agreed to
// anything, least of all a price ceiling they were part way through typing.
func TestQuittingSavesNothing(t *testing.T) {
	var called bool
	e := NewEditor("default", config.Profile{})
	e.Save = func(config.Profile) error { called = true; return nil }

	e = sendE(e, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if called {
		t.Fatal("quitting wrote the profile")
	}
	if done, saved := e.Done(); !done || saved {
		t.Errorf("done=%v saved=%v", done, saved)
	}
}

// A write that fails must keep the editor open and say so, rather than report
// success and lose the work.
func TestFailedSaveKeepsTheEditorOpen(t *testing.T) {
	e := NewEditor("default", config.Profile{})
	e.Save = func(config.Profile) error { return errWrite }
	e = sendE(e, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})

	if done, saved := e.Done(); done || saved {
		t.Error("a failed write was reported as a save")
	}
	if !strings.Contains(e.View(), "disk full") {
		t.Error("the failure was not shown")
	}
}

// The preview is the reason to edit here rather than by re-running a command
// with flags: the market answers immediately and nothing is spent.
func TestPreviewShowsWhatWouldBeRentAndSaysNothingIsSpent(t *testing.T) {
	e := NewEditor("default", config.Profile{Model: "org/m"})
	e = sendE(e, PreviewMsg{Rows: []PreviewRow{
		{GPU: "RTX 4090", VRAMGB: 24, PriceHr: 0.34, Reliability: 0.98, Selected: true},
		{GPU: "A100", VRAMGB: 80, PriceHr: 1.29, Reliability: 0.99},
	}})
	v := e.View()
	for _, want := range []string{"RTX 4090", "$0.340/hr", "A100", "nothing is spent"} {
		if !strings.Contains(v, want) {
			t.Errorf("preview omits %q", want)
		}
	}
}

// Empty criteria are a configuration problem, and the operator needs pointing
// at the setting to loosen rather than left to guess.
func TestEmptyPreviewSuggestsWhatToLoosen(t *testing.T) {
	e := NewEditor("default", config.Profile{Model: "org/m"})
	e = sendE(e, PreviewMsg{Rows: nil})
	v := e.View()
	if !strings.Contains(v, "no offer satisfies") {
		t.Error("an empty market was not reported")
	}
	if !strings.Contains(v, "price ceiling") {
		t.Error("gave no hint about which setting is too tight")
	}
}

var errWrite = errDiskFull{}

type errDiskFull struct{}

func (errDiskFull) Error() string { return "disk full" }
