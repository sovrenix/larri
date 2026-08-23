// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"fmt"
	"strings"
)

func (e Editor) View() string {
	var b strings.Builder
	b.WriteString(title.Render("🦞 larri config"))
	b.WriteString(dim.Render("  profile " + e.Name))
	b.WriteString("\n" + dim.Render(strings.Repeat("─", min(e.width, 72))) + "\n\n")

	for i, f := range e.fields {
		cursor := "  "
		name := label.Render(fmt.Sprintf("%-16s", f.label))
		if i == e.cursor {
			cursor = title.Render("▸ ")
			name = value.Render(fmt.Sprintf("%-16s", f.label))
		}
		val := f.get(&e.Profile)
		shown := dim.Render("(any)")
		if val != "" {
			shown = value.Render(val)
			if f.money {
				// A limit is the one field where being wrong costs money in
				// both directions, so it never renders as ordinary text.
				shown = money.Render(val)
			}
		}
		if e.typing && i == e.cursor {
			shown = value.Render(e.buf) + title.Render("▏")
		}
		b.WriteString(cursor + name + shown + "\n")
		if i == e.cursor {
			b.WriteString("    " + dim.Render(f.help) + "\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(e.previewView())

	if e.status != "" {
		b.WriteString("\n  " + bad.Render(e.status) + "\n")
	}
	b.WriteString("\n" + dim.Render(strings.Repeat("─", min(e.width, 72))) + "\n")
	if e.typing {
		b.WriteString(dim.Render("  enter  accept     esc  cancel"))
	} else {
		b.WriteString(dim.Render("  ↑↓ move   enter edit   r refresh   s save   q quit without saving"))
	}
	return b.String()
}

// previewView shows what the current criteria would actually rent.
//
// This is the reason to edit criteria here rather than by re-running a command
// with different flags: the market answers immediately, and it costs a search
// rather than a rental.
func (e Editor) previewView() string {
	var b strings.Builder
	switch {
	case e.Profile.Model == "":
		b.WriteString(dim.Render("  set a model to see what it would rent\n"))
		return b.String()
	case e.loading:
		b.WriteString(dim.Render("  searching…\n"))
		return b.String()
	case e.previewE != "":
		b.WriteString("  " + warn.Render(truncate(e.previewE, e.width-4)) + "\n")
		return b.String()
	case len(e.preview) == 0:
		b.WriteString("  " + warn.Render("no offer satisfies these criteria") + "\n")
		b.WriteString(dim.Render("  loosen the price ceiling, the gpu filter, or the reliability floor\n"))
		return b.String()
	}

	b.WriteString(label.Render("  what this would rent") + dim.Render("  (nothing is spent)") + "\n")
	for i, r := range e.preview {
		if i >= 5 {
			break
		}
		mark := "   "
		if r.Selected {
			mark = title.Render(" → ")
		}
		b.WriteString(fmt.Sprintf("%s%-18s %-6s %s  %s\n",
			mark, r.GPU, fmt.Sprintf("%dGB", r.VRAMGB),
			money.Render(fmt.Sprintf("$%.3f/hr", r.PriceHr)),
			dim.Render(fmt.Sprintf("rel %.2f", r.Reliability))))
	}
	return b.String()
}
