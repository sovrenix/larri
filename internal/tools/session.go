// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tools

import (
	"context"
	"sync"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
)

// Session owns the rig a long-running surface is serving.
//
// It exists because an MCP server outlives its tool calls, and that changes
// what `larri_up` can mean. Without somewhere to keep the tunnel, the tool can
// only provision — leaving an instance billing by the second with no runtime
// on it, which is renting hardware that cannot serve. With a session, the
// server holds the rig exactly as `larri up` holds it in the foreground.
//
// Bring-up runs in the background and is polled, rather than blocking the call
// that started it. A rig can take twenty minutes to come up; a tool call that
// blocked for twenty minutes would be indistinguishable from a hung server,
// and the agent would have no way to ask what was happening.
type Session struct {
	mu      sync.Mutex
	rigID   string
	phase   string
	message string
	live    *daemon.Live
	err     error
	running bool
	started time.Time
	cancel  context.CancelFunc
}

// Snapshot is what a caller can see of the session without holding its lock.
type Snapshot struct {
	RigID    string
	Phase    string
	Message  string
	Endpoint string
	Token    string
	Err      error
	Running  bool
	Elapsed  time.Duration
}

// Begin claims the session for a rig, refusing if one is already in flight.
//
// Refusing is the point: two concurrent bring-ups mean two billing instances,
// and an agent is likelier than a person to ask twice.
func (s *Session) Begin(rigID string, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.rigID, s.cancel, s.running = rigID, cancel, true
	s.phase, s.message, s.err, s.live = "starting", "", nil, nil
	s.started = time.Now()
	return true
}

// Note records progress, so a poll can answer what is happening rather than
// only whether it finished.
func (s *Session) Note(phase, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phase, s.message = phase, message
}

// Ready records a rig that is serving.
func (s *Session) Ready(live *daemon.Live) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live, s.phase, s.message = live, "ready", "serving"
}

// Fail records why a bring-up ended without serving.
func (s *Session) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err, s.running, s.phase = err, false, "failed"
	s.live = nil
}

// Finish releases the session after a teardown.
func (s *Session) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running, s.live, s.phase = false, nil, "done"
}

// Live returns the serving rig, or nil.
func (s *Session) Live() *daemon.Live {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// RigID returns the rig this session is holding, or "".
func (s *Session) RigID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rigID
}

// Snapshot reports the session's state.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		RigID: s.rigID, Phase: s.phase, Message: s.message,
		Err: s.err, Running: s.running,
	}
	if !s.started.IsZero() {
		// Unrounded: the session reports how long, and the surface decides
		// how to render it. Rounding here would make the value depend on the
		// display it happens to be destined for.
		snap.Elapsed = time.Since(s.started)
	}
	if s.live != nil {
		snap.Endpoint = s.live.Endpoint
		snap.Token = s.live.ClientToken.Reveal()
	}
	return snap
}

// Stop cancels an in-flight bring-up and closes the tunnel.
//
// It does not destroy: teardown is a provider call with a termination record,
// and conflating "stop holding this" with "stop paying for this" is how a
// dropped session becomes an instance nobody is tracking.
func (s *Session) Stop() {
	s.mu.Lock()
	cancel, live := s.cancel, s.live
	s.cancel, s.live, s.running = nil, nil, false
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if live != nil {
		_ = live.Close()
	}
}

// HeldRig returns the rig a session is holding, if it matches the one given.
func (s *Session) HeldRig(rig *core.Rig) bool {
	return rig != nil && s.RigID() == rig.ID
}
