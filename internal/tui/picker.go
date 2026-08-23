// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"go.sovrenix.com/larri/internal/config"
)

// Profiles lists the saved profiles and hands one to the editor.
//
// This one *is* a parent of the editor, unlike the dashboard: picking a
// profile and editing it are two steps of the same task, so composing them is
// modelling the workflow rather than merging two unrelated ones.
//
// It exists because naming a profile on the command line is a poor way to
// reach something you cannot see. Without a list, `--profile codr` is
// indistinguishable from `--profile coder` until the rig comes up wrong.
type Profiles struct {
	profiles map[string]config.Profile
	names    []string
	cursor   int

	editor  *Editor
	naming  bool
	buf     string
	confirm bool
	status  string
	width   int
	done    bool
	dirty   bool

	// Save persists the whole set, so deleting is as much a save as editing.
	Save func(map[string]config.Profile) error
	// Preview is handed to the editor.
	Preview func(config.Profile) tea.Cmd
}

// NewProfiles builds a picker over a set of profiles.
func NewProfiles(profiles map[string]config.Profile) Profiles {
	if profiles == nil {
		profiles = map[string]config.Profile{}
	}
	p := Profiles{profiles: profiles, width: 80}
	p.resort()
	return p
}

// resort keeps the list in a stable order.
//
// Map iteration order in Go is deliberately random, so a list built from one
// would reshuffle on every keystroke and the cursor would point at a different
// profile each time it was drawn.
func (p *Profiles) resort() {
	p.names = p.names[:0]
	for n := range p.profiles {
		p.names = append(p.names, n)
	}
	sort.Strings(p.names)
	if p.cursor >= len(p.names) {
		p.cursor = max0(len(p.names) - 1)
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Done reports that the picker has finished, and whether anything was written.
func (p Profiles) Done() (bool, bool) { return p.done, p.dirty }

// Result returns the profile set as it now stands.
func (p Profiles) Result() map[string]config.Profile { return p.profiles }

func (p Profiles) Init() tea.Cmd { return nil }

func (p Profiles) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// While the editor is open it owns the keyboard entirely. Sharing input
	// between a list and a text field is how a keystroke meant for one ends
	// up deleting something in the other.
	if p.editor != nil {
		return p.updateEditor(msg)
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width = msg.Width
		return p, nil
	case tea.KeyMsg:
		if p.naming {
			return p.nameKey(msg)
		}
		if p.confirm {
			return p.confirmKey(msg)
		}
		return p.listKey(msg)
	}
	return p, nil
}

func (p Profiles) updateEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := p.editor.Update(msg)
	ed := next.(Editor)
	p.editor = &ed
	if done, saved := ed.Done(); done {
		if saved {
			p.profiles[ed.Name] = ed.Result()
			p.dirty = true
			p.status = "saved " + ed.Name
		}
		p.editor = nil
		p.resort()
		// The editor asked to quit; the picker is still running, so that
		// request must not propagate or closing an edit would close the
		// program.
		return p, nil
	}
	return p, cmd
}

func (p Profiles) listKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "j":
		if p.cursor < len(p.names)-1 {
			p.cursor++
		}
	case "enter", "e":
		if len(p.names) == 0 {
			p.status = "! no profiles yet — press n to make one"
			return p, nil
		}
		return p.edit(p.names[p.cursor])
	case "n":
		p.naming, p.buf, p.status = true, "", ""
	case "d":
		if len(p.names) == 0 {
			return p, nil
		}
		p.confirm = true
	case "s":
		return p.persist()
	case "q", "esc", "ctrl+c":
		if p.dirty {
			// Unsaved edits are the operator's work; discarding them without
			// a word is the one thing a picker must never do.
			if err := p.write(); err != nil {
				p.status = "! " + err.Error()
				return p, nil
			}
		}
		p.done = true
		return p, tea.Quit
	}
	return p, nil
}

func (p Profiles) edit(name string) (tea.Model, tea.Cmd) {
	ed := NewEditor(name, p.profiles[name])
	ed.Preview = p.Preview
	// The editor saves into the picker's map rather than to disk: one writer
	// (FR-CFG-04), and deleting a profile has to go through the same path.
	ed.Save = func(config.Profile) error { return nil }
	p.editor = &ed
	p.status = ""
	return p, ed.Init()
}

func (p Profiles) nameKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		name := strings.TrimSpace(p.buf)
		p.naming = false
		switch {
		case name == "":
			return p, nil
		case p.profiles[name].Model != "" || hasKey(p.profiles, name):
			p.status = "! " + name + " already exists"
			return p, nil
		}
		p.profiles[name] = config.Profile{}
		p.dirty = true
		p.resort()
		for i, n := range p.names {
			if n == name {
				p.cursor = i
			}
		}
		return p.edit(name)
	case "esc":
		p.naming = false
	case "backspace":
		if p.buf != "" {
			p.buf = p.buf[:len(p.buf)-1]
		}
	default:
		if len(msg.String()) == 1 {
			p.buf += msg.String()
		}
	}
	return p, nil
}

func (p Profiles) confirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p.confirm = false
	if msg.String() != "y" && msg.String() != "Y" {
		return p, nil
	}
	name := p.names[p.cursor]
	delete(p.profiles, name)
	p.dirty = true
	p.status = "deleted " + name
	p.resort()
	return p, nil
}

func (p Profiles) persist() (tea.Model, tea.Cmd) {
	if err := p.write(); err != nil {
		p.status = "! " + err.Error()
		return p, nil
	}
	p.status = "written"
	p.dirty = false
	return p, nil
}

func (p *Profiles) write() error {
	if p.Save == nil {
		return fmt.Errorf("nothing to save to")
	}
	return p.Save(p.profiles)
}

func hasKey(m map[string]config.Profile, k string) bool {
	_, ok := m[k]
	return ok
}

func (p Profiles) View() string {
	if p.editor != nil {
		return p.editor.View()
	}
	var b strings.Builder
	b.WriteString(title.Render("🦞 larri config"))
	b.WriteString(dim.Render("  saved profiles"))
	b.WriteString("\n" + dim.Render(strings.Repeat("─", min(p.width, 72))) + "\n\n")

	if len(p.names) == 0 {
		b.WriteString(dim.Render("  no profiles yet\n"))
	}
	for i, n := range p.names {
		cursor, name := "  ", label.Render(fmt.Sprintf("%-14s", n))
		if i == p.cursor {
			cursor, name = title.Render("▸ "), value.Render(fmt.Sprintf("%-14s", n))
		}
		suffix := ""
		if n == config.DefaultProfile {
			// Worth marking: this is the one a bare `larri up` applies.
			suffix = dim.Render("   ← used by a bare `larri up`")
		}
		b.WriteString(cursor + name + dim.Render(p.profiles[n].Summary()) + suffix + "\n")
	}
	if p.naming {
		b.WriteString("\n  " + label.Render("new profile name  ") + value.Render(p.buf) + title.Render("▏") + "\n")
	}
	if p.confirm && len(p.names) > 0 {
		b.WriteString("\n  " + bad.Render("delete "+p.names[p.cursor]+"? [y/N]") + "\n")
	}
	if p.status != "" {
		st := dim
		if strings.HasPrefix(p.status, "!") {
			st = bad
		}
		b.WriteString("\n  " + st.Render(p.status) + "\n")
	}
	b.WriteString("\n" + dim.Render(strings.Repeat("─", min(p.width, 72))) + "\n")
	switch {
	case p.naming:
		b.WriteString(dim.Render("  enter  create     esc  cancel"))
	default:
		b.WriteString(dim.Render("  ↑↓ move   enter edit   n new   d delete   s write   q save and quit"))
	}
	return b.String()
}
