// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/state"
	"go.sovrenix.com/larri/internal/term"
	"go.sovrenix.com/larri/internal/tui"
)

// tuiModel wraps the dashboard so the command can hand back a printable
// summary after the alternate screen closes.
type tuiModel struct{ tui.Model }

func newTUIModel() tuiModel { return tuiModel{tui.New()} }

func (m tuiModel) Init() term.Cmd { return m.Model.Init() }

func (m tuiModel) Update(msg term.Msg) (term.Model, term.Cmd) {
	inner, cmd := m.Model.Update(msg)
	m.Model = inner.(tui.Model)
	return m, cmd
}

func (m tuiModel) View() string { return m.Model.View() }

// Summary is what remains on the scrollback once the dashboard is gone. The
// cost of a rig should not disappear with the screen that showed it.
func (m tuiModel) Summary() string { return "\n" + m.Model.Done() }

func tuiEvent(e daemon.Event) term.Msg { return tui.EventMsg(e) }
func tuiReady(r *core.Rig, ep, tok string) term.Msg {
	return tui.ReadyMsg{Rig: r, Endpoint: ep, Token: tok}
}
func tuiDone(t *core.Termination, c core.CostSummary, err error) term.Msg {
	return tui.DoneMsg{Term: t, Cost: c, Err: err}
}

// sampleInto feeds the dashboard from the journal and the proxy, never from
// numbers the screen keeps for itself (P5).
func sampleInto(ctx context.Context, prog *term.Program, o *daemon.Orchestrator, live *daemon.Live) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		now := time.Now()
		entries, err := o.Store.Entries()
		if err != nil {
			continue
		}
		act := live.Activity()
		msg := tui.StatsMsg{
			Accrued: state.CostFor(entries, live.Rig.ID, now),
			Healthy: live.Rig.State == core.StateReady,
		}
		if act != nil {
			msg.Idle = act.IdleFor(now)
			msg.Requests = act.Requests()
			msg.Probes = act.Probes()
			msg.InFlight = act.InFlight()
		}
		prog.Send(msg)
	}
}
