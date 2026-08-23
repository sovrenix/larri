// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"go.sovrenix.com/larri/internal/config"
)

// Editor edits a criteria profile and previews what it would rent.
//
// It is a separate model from the dashboard rather than a mode inside it. The
// two have nothing in common — one watches a rig that exists, the other shapes
// one that does not — and folding them into a single Update would put two
// unrelated state machines in one switch, where the only thing keeping them
// apart is a discipline nobody can see.
//
// It edits and previews. It does not save: writing configuration belongs in
// core, and a surface that owned it would be the second place the rules live
// (FR-CFG-04). The caller supplies Save.
type Editor struct {
	Name    string
	Profile config.Profile

	fields []field
	cursor int
	typing bool
	buf    string
	status string

	// Preview is asked to rank the market for the current profile. It costs a
	// search and spends nothing, which is what makes editing worth doing here
	// rather than by re-running a command with different flags.
	Preview func(config.Profile) tea.Cmd
	// Save persists the profile. Supplied by the caller so this model never
	// writes configuration itself.
	Save func(config.Profile) error

	preview  []PreviewRow
	previewE string
	loading  bool
	width    int
	done     bool
	saved    bool
}

// PreviewRow is one offer the current criteria would consider.
type PreviewRow struct {
	GPU         string
	VRAMGB      int
	PriceHr     float64
	Reliability float64
	Selected    bool
}

// PreviewMsg carries a completed preview.
type PreviewMsg struct {
	Rows []PreviewRow
	VRAM string
	Err  error
}

// field is one editable setting.
type field struct {
	label string
	help  string
	get   func(*config.Profile) string
	set   func(*config.Profile, string) error
	// money marks a setting that limits spending, so the view can say so.
	money bool
}

// NewEditor builds an editor over a profile.
func NewEditor(name string, p config.Profile) Editor {
	return Editor{Name: name, Profile: p, fields: profileFields(), width: 80}
}

func profileFields() []field {
	num := func(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(s), 64) }
	return []field{
		{
			label: "model", help: "hugging face ref, gguf repo, or ollama tag",
			get: func(p *config.Profile) string { return p.Model },
			set: func(p *config.Profile, v string) error { p.Model = strings.TrimSpace(v); return nil },
		},
		{
			label: "quantization", help: "fp16, q4_K_M, awq — ollama tags carry their own",
			get: func(p *config.Profile) string { return p.Quantization },
			set: func(p *config.Profile, v string) error { p.Quantization = strings.TrimSpace(v); return nil },
		},
		{
			label: "context", help: "tokens; larger costs vram linearly in the kv cache",
			get: func(p *config.Profile) string { return itoa(p.ContextLen) },
			set: func(p *config.Profile, v string) error {
				n, err := atoiOrZero(v)
				if err != nil {
					return err
				}
				p.ContextLen = n
				return nil
			},
		},
		{
			label: "gpu", help: "comma-separated filter, e.g. RTX 4090,A100 — empty means any",
			get: func(p *config.Profile) string { return strings.Join(p.GPUModel, ",") },
			set: func(p *config.Profile, v string) error {
				p.GPUModel = splitList(v)
				return nil
			},
		},
		{
			label: "max price $/hr", money: true,
			help: "ceiling; a stale one fails as 'no offer satisfies the criteria'",
			get:  func(p *config.Profile) string { return ftoa(p.MaxPriceHr) },
			set: func(p *config.Profile, v string) error {
				if strings.TrimSpace(v) == "" {
					p.MaxPriceHr = 0
					return nil
				}
				f, err := num(v)
				if err != nil || f < 0 {
					return fmt.Errorf("want a price in dollars per hour")
				}
				p.MaxPriceHr = f
				return nil
			},
		},
		{
			label: "min reliability", help: "0..1; the provider's own score, which predicts less than you would hope",
			get: func(p *config.Profile) string { return ftoa(p.MinReliability) },
			set: func(p *config.Profile, v string) error {
				if strings.TrimSpace(v) == "" {
					p.MinReliability = 0
					return nil
				}
				f, err := num(v)
				if err != nil || f < 0 || f > 1 {
					return fmt.Errorf("want a number between 0 and 1")
				}
				p.MinReliability = f
				return nil
			},
		},
		{
			label: "disk gb", help: "must hold the image and the weights",
			get: func(p *config.Profile) string { return itoa(p.DiskGB) },
			set: func(p *config.Profile, v string) error {
				n, err := atoiOrZero(v)
				if err != nil {
					return err
				}
				p.DiskGB = n
				return nil
			},
		},
		{
			label: "local port", help: "the address clients are wired to and never rewired from",
			get: func(p *config.Profile) string { return itoa(p.LocalPort) },
			set: func(p *config.Profile, v string) error {
				n, err := atoiOrZero(v)
				if err != nil || n < 0 || n > 65535 {
					return fmt.Errorf("want a port between 0 and 65535")
				}
				p.LocalPort = n
				return nil
			},
		},
		{
			label: "runtime", help: "vllm, llamacpp, ollama — empty lets larri choose from the model",
			get: func(p *config.Profile) string { return p.Runtime },
			set: func(p *config.Profile, v string) error {
				v = strings.TrimSpace(v)
				switch v {
				case "", "vllm", "llamacpp", "ollama":
					p.Runtime = v
					return nil
				}
				return fmt.Errorf("want vllm, llamacpp, ollama, or empty")
			},
		},
	}
}

func (e Editor) Init() tea.Cmd { return e.refresh() }

// Done reports that the editor has finished, and whether anything was saved.
func (e Editor) Done() (bool, bool) { return e.done, e.saved }

// Result returns the edited profile.
func (e Editor) Result() config.Profile { return e.Profile }

func (e Editor) refresh() tea.Cmd {
	if e.Preview == nil || e.Profile.Model == "" {
		return nil
	}
	return e.Preview(e.Profile)
}

func (e Editor) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		e.width = msg.Width
		return e, nil

	case PreviewMsg:
		e.loading = false
		e.preview, e.previewE = msg.Rows, ""
		if msg.Err != nil {
			e.previewE = msg.Err.Error()
			e.preview = nil
		}
		return e, nil

	case tea.KeyMsg:
		if e.typing {
			return e.editKey(msg)
		}
		return e.navKey(msg)
	}
	return e, nil
}

func (e Editor) editKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		f := e.fields[e.cursor]
		if err := f.set(&e.Profile, e.buf); err != nil {
			e.status = "! " + err.Error()
			return e, nil
		}
		e.typing, e.status = false, ""
		e.loading = true
		return e, e.refresh()
	case "esc":
		e.typing, e.status = false, ""
		return e, nil
	case "backspace":
		if e.buf != "" {
			e.buf = e.buf[:len(e.buf)-1]
		}
		return e, nil
	default:
		if len(msg.String()) == 1 {
			e.buf += msg.String()
		} else if msg.Type == tea.KeySpace {
			e.buf += " "
		}
		return e, nil
	}
}

func (e Editor) navKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if e.cursor > 0 {
			e.cursor--
		}
	case "down", "j":
		if e.cursor < len(e.fields)-1 {
			e.cursor++
		}
	case "enter":
		e.typing = true
		e.buf = e.fields[e.cursor].get(&e.Profile)
		e.status = ""
	case "s":
		if e.Save == nil {
			e.status = "! nothing to save to"
			return e, nil
		}
		if err := e.Save(e.Profile); err != nil {
			e.status = "! " + err.Error()
			return e, nil
		}
		e.saved, e.done = true, true
		return e, tea.Quit
	case "r":
		e.loading = true
		return e, e.refresh()
	case "q", "esc", "ctrl+c":
		e.done = true
		return e, tea.Quit
	}
	return e, nil
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func ftoa(f float64) string {
	if f == 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func atoiOrZero(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("want a whole number")
	}
	return n, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
