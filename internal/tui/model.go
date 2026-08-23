// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package tui renders a rig's life as a terminal dashboard.
//
// It holds no state of its own (P5): everything on screen is either an event
// the orchestrator emitted or a value read from the rig and the journal. A TUI
// that computed its own cost would eventually disagree with `larri status`,
// and the operator would have no way to know which to believe.
package tui

import (
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/term"
)

// Phase is where the rig is in its life, as far as the screen is concerned.
type Phase int

const (
	PhaseProvisioning Phase = iota
	PhaseServing
	PhaseTearingDown
	PhaseDone
)

// EventMsg carries one orchestrator event.
type EventMsg daemon.Event

// ReadyMsg says the rig is serving.
type ReadyMsg struct {
	Rig      *core.Rig
	Endpoint string
	Token    string
}

// StatsMsg is a periodic sample of what the rig is doing and costing.
type StatsMsg struct {
	Accrued  core.CostSummary
	Idle     time.Duration
	Requests int64
	Probes   int64
	InFlight int64
	Healthy  bool
}

// DoneMsg ends the session.
type DoneMsg struct {
	Term *core.Termination
	Cost core.CostSummary
	Err  error
}

// tickMsg drives the elapsed clock between stat samples.
type tickMsg time.Time

// Model is the dashboard.
type Model struct {
	Phase   Phase
	Rig     *core.Rig
	Started time.Time

	endpoint string
	token    string

	events  []daemon.Event
	stats   StatsMsg
	term    *core.Termination
	err     error
	confirm bool // destroy confirmation is showing

	width int

	// Destroy is called when the operator confirms a teardown. The TUI does
	// not destroy anything itself — it asks, and the caller acts, so the
	// screen never claims a rig is gone before the provider agrees.
	Destroy func()
	// Quit is called when the operator leaves without destroying.
	Quit func()
}

func New() Model { return Model{Started: time.Now(), width: 80} }

func (m Model) Init() term.Cmd { return tick() }

func tick() term.Cmd {
	return term.Tick(time.Second, func(t time.Time) term.Msg { return tickMsg(t) })
}

func (m Model) Update(msg term.Msg) (term.Model, term.Cmd) {
	switch msg := msg.(type) {
	case term.SizeMsg:
		m.width = msg.Width
		return m, nil

	case tickMsg:
		return m, tick()

	case EventMsg:
		// Bounded: a long weight download emits hundreds of lines and the
		// screen only ever shows the tail.
		m.events = append(m.events, daemon.Event(msg))
		if len(m.events) > 200 {
			m.events = m.events[len(m.events)-200:]
		}
		return m, nil

	case ReadyMsg:
		m.Phase = PhaseServing
		m.Rig = msg.Rig
		m.endpoint = msg.Endpoint
		m.token = msg.Token
		return m, nil

	case StatsMsg:
		m.stats = msg
		return m, nil

	case DoneMsg:
		m.Phase = PhaseDone
		m.term = msg.Term
		m.stats.Accrued = msg.Cost
		m.err = msg.Err
		return m, term.Quit

	case term.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(msg term.KeyMsg) (term.Model, term.Cmd) {
	if m.confirm {
		switch msg.String() {
		case "y", "Y":
			m.confirm = false
			m.Phase = PhaseTearingDown
			if m.Destroy != nil {
				m.Destroy()
			}
			return m, nil
		default:
			m.confirm = false
			return m, nil
		}
	}
	switch msg.String() {
	case "d":
		if m.Phase == PhaseServing {
			m.confirm = true
		}
		return m, nil
	case "q", "ctrl+c", "esc":
		// Leaving is a teardown, not a detach. A TUI that exited while the
		// rig kept billing would make quitting look free.
		m.Phase = PhaseTearingDown
		if m.Quit != nil {
			m.Quit()
		}
		return m, nil
	}
	return m, nil
}
