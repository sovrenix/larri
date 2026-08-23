// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"fmt"
	"go.sovrenix.com/larri/internal/term"
	"strings"
	"time"
)

var (
	// Colours are chosen from the 256-colour cube and adapt to the terminal's
	// background, because a dashboard that is unreadable on a light terminal
	// is a dashboard that gets turned off.
	dim   = term.NewStyle().Foreground(term.Adaptive("244", "240"))
	label = term.NewStyle().Foreground(term.Adaptive("240", "245"))
	value = term.NewStyle().Bold(true)
	good  = term.NewStyle().Foreground(term.Adaptive("28", "78"))
	warn  = term.NewStyle().Foreground(term.Adaptive("130", "214"))
	bad   = term.NewStyle().Foreground(term.Adaptive("124", "203"))
	// Money is styled as a warning throughout, on purpose: the number that
	// matters here is the one that keeps growing.
	money = term.NewStyle().Foreground(term.Adaptive("130", "214")).Bold(true)
	title = term.NewStyle().Bold(true).Foreground(term.Adaptive("25", "39"))
)

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n")

	switch m.Phase {
	case PhaseProvisioning:
		b.WriteString(m.provisioning())
	case PhaseServing, PhaseTearingDown:
		b.WriteString(m.serving())
	case PhaseDone:
		b.WriteString(m.done())
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(m.footer())
	return b.String()
}

func (m Model) header() string {
	s := title.Render("🦞 L.A.R.R.I.")
	if m.Rig != nil {
		s += dim.Render("  rig " + m.Rig.ID)
	}
	return s + "\n" + dim.Render(strings.Repeat("─", min(m.width, 72)))
}

// provisioning shows the event tail, which during a bring-up is the only thing
// that distinguishes a 15 GB image pull from a hang.
func (m Model) provisioning() string {
	var b strings.Builder
	b.WriteString(label.Render("  provisioning") + "  " +
		dim.Render("elapsed "+time.Since(m.Started).Round(time.Second).String()) + "\n\n")

	tail := m.events
	if len(tail) > 12 {
		tail = tail[len(tail)-12:]
	}
	for _, e := range tail {
		mark := " "
		st := dim
		if e.Warning {
			mark, st = "!", warn
		}
		b.WriteString(fmt.Sprintf("  %s %-11s %s\n", st.Render(mark),
			label.Render(e.Phase), st.Render(truncate(e.Message, m.width-18))))
	}
	return b.String()
}

func (m Model) serving() string {
	var b strings.Builder
	row := func(k, v string) {
		b.WriteString("  " + label.Render(fmt.Sprintf("%-11s", k)) + v + "\n")
	}

	state := good.Render("READY")
	if m.Phase == PhaseTearingDown {
		state = warn.Render("TEARING DOWN")
	} else if !m.stats.Healthy && m.stats.Requests+m.stats.Probes > 0 {
		state = bad.Render("DEGRADED")
	}
	row("state", state)
	if m.Rig != nil {
		row("model", value.Render(m.Rig.Model.ServedName)+dim.Render("  ("+m.Rig.Model.Ref+")"))
		row("hardware", fmt.Sprintf("%s %s  %s",
			value.Render(m.Rig.Offer.GPUModel),
			dim.Render(fmt.Sprintf("%dGB", m.Rig.Offer.VRAMTotalGB())),
			money.Render(fmt.Sprintf("$%.3f/hr", m.Rig.Offer.PriceHr))))
		row("runtime", string(m.Rig.Runtime))
	}
	row("endpoint", value.Render(m.endpoint))
	if m.token != "" {
		row("key", dim.Render(m.token))
	}

	b.WriteString("\n")
	row("elapsed", time.Since(m.Started).Round(time.Second).String())

	// Compute and storage separately, because storage is the half that keeps
	// billing when the GPU is released and is the one that surprises people.
	c := m.stats.Accrued
	row("accrued", money.Render(fmt.Sprintf("$%.4f", c.TotalUSD))+
		dim.Render(fmt.Sprintf("  compute $%.4f · storage $%.4f", c.ComputeUSD, c.StorageUSD)))

	idle := "—"
	if m.stats.Idle > 0 {
		idle = m.stats.Idle.Round(time.Second).String()
	} else if m.stats.InFlight > 0 {
		idle = good.Render("serving")
	}
	row("idle", idle)
	row("requests", fmt.Sprintf("%d %s", m.stats.Requests,
		dim.Render(fmt.Sprintf("(+%d health probes, excluded from idle)", m.stats.Probes))))

	if m.confirm {
		b.WriteString("\n  " + bad.Render("destroy this rig? [y/N]") + "\n")
	}
	return b.String()
}

func (m Model) done() string {
	var b strings.Builder
	c := m.stats.Accrued
	if m.err != nil {
		b.WriteString("  " + bad.Render("failed: "+m.err.Error()) + "\n")
	}
	if m.term != nil {
		b.WriteString("  " + warn.Render(string(m.term.Code)) + "  " + m.term.Summary + "\n")
		for k, v := range m.term.Evidence {
			b.WriteString("    " + dim.Render(fmt.Sprintf("%-14s %s", k, v)) + "\n")
		}
	}
	b.WriteString("\n  " + good.Render("DESTROYED") +
		fmt.Sprintf("  ran %s  total %s\n",
			c.Ran.Round(time.Second), money.Render(fmt.Sprintf("$%.4f", c.TotalUSD))))
	b.WriteString(dim.Render("  absence confirmed at the provider; billing has stopped") + "\n")
	return b.String()
}

func (m Model) footer() string {
	keys := "q  tear down and quit"
	if m.Phase == PhaseServing {
		keys = "d  destroy     q  tear down and quit"
	}
	return dim.Render(strings.Repeat("─", min(m.width, 72))) + "\n" + dim.Render("  "+keys)
}

func truncate(s string, n int) string {
	if n < 8 {
		n = 8
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Done renders the closing summary for printing outside the alternate screen.
// The cost of a rig should not vanish with the dashboard that showed it.
func (m Model) Done() string { return m.done() }
