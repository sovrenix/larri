// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/term"
)

func sendP(p Profiles, msgs ...term.Msg) Profiles {
	for _, m := range msgs {
		next, _ := p.Update(m)
		p = next.(Profiles)
	}
	return p
}

func key(s string) term.Msg {
	switch s {
	case "enter":
		return term.KeyMsg{Type: term.KeyEnter}
	case "esc":
		return term.KeyMsg{Type: term.KeyEsc}
	}
	return term.KeyMsg{Type: term.KeyRunes, Runes: []rune(s)}
}

func typeName(s string) []term.Msg {
	out := []term.Msg{key("n")}
	for _, r := range s {
		out = append(out, key(string(r)))
	}
	return append(out, key("enter"))
}

func threeProfiles() map[string]config.Profile {
	return map[string]config.Profile{
		"default": {Model: "org/small"},
		"coder":   {Model: "org/coder", MaxPriceHr: 0.6},
		"big":     {Model: "org/big", MaxPriceHr: 2},
	}
}

// Go randomises map iteration, so a list built from one would reshuffle on
// every keystroke and the cursor would land on a different profile each time.
func TestProfilesAreListedInAStableOrder(t *testing.T) {
	p := NewProfiles(threeProfiles())
	first := p.View()
	for i := 0; i < 8; i++ {
		if NewProfiles(threeProfiles()).View() != first {
			t.Fatal("the listing reorders between builds")
		}
	}
	v := first
	ib, ic, id := strings.Index(v, "big"), strings.Index(v, "coder"), strings.Index(v, "default")
	if !(ib < ic && ic < id) {
		t.Errorf("not sorted: big=%d coder=%d default=%d", ib, ic, id)
	}
}

// The default profile is the one a bare `larri up` applies, which is worth
// saying where the operator is choosing between them.
func TestDefaultProfileIsMarkedInTheList(t *testing.T) {
	v := NewProfiles(threeProfiles()).View()
	if !strings.Contains(v, "used by a bare") {
		t.Error("nothing marks which profile applies without --profile")
	}
}

func TestEditingAProfileWritesItBack(t *testing.T) {
	p := NewProfiles(threeProfiles())
	p = sendP(p, key("j"))     // big → coder
	p = sendP(p, key("enter")) // edit
	p = sendP(p, replaceWith("org/edited")...)
	p = sendP(p, key("s")) // save inside the editor

	if got := p.Result()["coder"].Model; got != "org/edited" {
		t.Errorf("coder model = %q", got)
	}
	if _, dirty := p.Done(); !dirty {
		t.Error("the edit was not recorded as unsaved work")
	}
}

// Closing the editor must return to the list, not end the program. The
// editor asks to quit; that request belongs to it, not to its parent.
func TestClosingTheEditorReturnsToTheList(t *testing.T) {
	p := NewProfiles(threeProfiles())
	p = sendP(p, key("enter"), key("q"))
	if done, _ := p.Done(); done {
		t.Fatal("leaving the editor closed the whole picker")
	}
	if !strings.Contains(p.View(), "saved profiles") {
		t.Error("did not return to the list")
	}
}

func TestNewProfileIsCreatedAndOpened(t *testing.T) {
	p := NewProfiles(threeProfiles())
	p = sendP(p, typeName("tiny")...)
	if _, ok := p.Result()["tiny"]; !ok {
		t.Fatal("the new profile was not created")
	}
	// It opens straight into the editor, since an empty profile is not
	// something anyone wants to look at in a list.
	if !strings.Contains(p.View(), "model") {
		t.Error("did not open the editor on the new profile")
	}
}

// Silently overwriting an existing profile with an empty one would destroy
// work with a single keystroke.
func TestNewRefusesAnExistingName(t *testing.T) {
	p := NewProfiles(threeProfiles())
	p = sendP(p, typeName("coder")...)
	if got := p.Result()["coder"].Model; got != "org/coder" {
		t.Errorf("coder was overwritten: %q", got)
	}
	if !strings.Contains(p.View(), "already exists") {
		t.Error("the clash was not reported")
	}
}

// Deleting takes a confirmation, like destroying a rig does.
func TestDeleteNeedsConfirmation(t *testing.T) {
	p := NewProfiles(threeProfiles())
	p = sendP(p, key("d"))
	if len(p.Result()) != 3 {
		t.Fatal("one keypress deleted a profile")
	}
	if !strings.Contains(p.View(), "delete big?") {
		t.Error("no confirmation was shown")
	}
	p = sendP(p, key("n"))
	if len(p.Result()) != 3 {
		t.Fatal("declining still deleted it")
	}
	p = sendP(p, key("d"), key("y"))
	if _, still := p.Result()["big"]; still {
		t.Error("confirming did not delete")
	}
}

// Unsaved edits are the operator's work; quitting must not discard them
// without a word.
func TestQuittingWritesPendingChanges(t *testing.T) {
	var written map[string]config.Profile
	p := NewProfiles(threeProfiles())
	p.Save = func(set map[string]config.Profile) error { written = set; return nil }

	p = sendP(p, key("d"), key("y")) // delete big
	p = sendP(p, key("q"))
	if written == nil {
		t.Fatal("quitting discarded an unsaved deletion")
	}
	if _, still := written["big"]; still {
		t.Error("the deletion was not written")
	}
}

// A failed write keeps the picker open rather than reporting success and
// losing the work.
func TestFailedWriteKeepsThePickerOpen(t *testing.T) {
	p := NewProfiles(threeProfiles())
	p.Save = func(map[string]config.Profile) error { return errWrite }
	p = sendP(p, key("d"), key("y"), key("q"))

	if done, _ := p.Done(); done {
		t.Error("closed despite failing to write")
	}
	if !strings.Contains(p.View(), "disk full") {
		t.Error("the failure was not shown")
	}
}

// replaceWith opens a field, clears the value it is pre-filled with, and types
// a new one.
//
// The pre-fill is deliberate — opening a field shows what it currently holds
// so it can be amended rather than retyped — so a test that only typed would
// be appending.
func replaceWith(s string) []term.Msg {
	out := []term.Msg{term.KeyMsg{Type: term.KeyEnter}}
	for i := 0; i < 64; i++ {
		out = append(out, term.KeyMsg{Type: term.KeyBackspace})
	}
	for _, r := range s {
		out = append(out, term.KeyMsg{Type: term.KeyRunes, Runes: []rune{r}})
	}
	return append(out, term.KeyMsg{Type: term.KeyEnter})
}
