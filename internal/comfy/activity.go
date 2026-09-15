// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// CountsAsWork reports whether a proxied request is the operator using the rig.
//
// Idle reclamation is on by default and it destroys (§4), so what counts as
// activity decides when a GPU stops being paid for. On a /v1 endpoint the
// question does not arise: a request is a completion and a completion is the
// work. A browser changes that completely. ComfyUI's frontend reconnects a
// WebSocket, re-polls its queue, and refetches thumbnails for as long as the
// tab is open — on a laptop left open overnight, indefinitely — so counting
// every request would mean the idle timeout never fires and the protection it
// implements is decorative.
//
// The rule is therefore about intent rather than traffic: a request that asks
// the GPU to do something is work, and a request that asks what the GPU has
// already done is not. Queueing a render counts; watching one does not.
//
// Erring toward not-work is the safe direction here only because the queue is
// watched separately — see HoldWhileBusy. Without that, a graph that renders
// for longer than the idle timeout would be destroyed halfway through, which
// is the expensive failure this pair exists to prevent.
func CountsAsWork(r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && path == "/prompt":
		return true // queueing a render
	case r.Method == http.MethodPost && path == "/interrupt":
		return true // stopping one
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/upload"):
		return true // supplying an input image
	case r.Method == http.MethodPost && path == "/queue":
		return true // clearing or reordering it
	}
	return false
}

// QueueState is how much work ComfyUI has outstanding.
type QueueState struct {
	Running int
	Pending int
}

// Busy reports whether the GPU has something to do.
func (q QueueState) Busy() bool { return q.Running > 0 || q.Pending > 0 }

// Queue asks what is outstanding.
func (c *Client) Queue(ctx context.Context) (QueueState, error) {
	raw, code, err := c.do(ctx, http.MethodGet, "/queue", nil)
	if err != nil {
		return QueueState{}, err
	}
	if code != http.StatusOK {
		return QueueState{}, nil
	}
	var doc struct {
		Running []json.RawMessage `json:"queue_running"`
		Pending []json.RawMessage `json:"queue_pending"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return QueueState{}, err
	}
	return QueueState{Running: len(doc.Running), Pending: len(doc.Pending)}, nil
}

// Holder is the part of wire.Activity this package needs, taken as an
// interface so comfy does not depend on the proxy.
type Holder interface {
	EnterInFlight()
	ExitInFlight()
}

// HoldWhileBusy keeps the idle clock stopped while ComfyUI has work queued.
//
// This closes the gap CountsAsWork leaves open, and the gap is not small. A
// render is submitted in one short POST and then runs for minutes — a large
// batch, a slow sampler, or an upscale chain for considerably longer than
// that — while no HTTP request is outstanding at all, because the browser
// watches progress over a WebSocket. To an idle timer measuring requests, a
// rig working flat out on a forty-minute graph is indistinguishable from an
// abandoned one, and the default action on idle is destroy.
//
// So the queue is polled and the in-flight count is held up while it is
// non-empty. The bracket is the mechanism wire already provides for exactly
// this — a supervisor holding a rig alive across work it knows about.
//
// A failed poll releases the hold rather than extending it. That direction is
// deliberate: an unreachable ComfyUI is a rig in trouble, and the other
// supervision paths are better placed to decide what to do about it than a
// helper whose only move is to keep paying.
func HoldWhileBusy(ctx context.Context, c *Client, h Holder, every time.Duration) {
	if every <= 0 {
		every = 15 * time.Second
	}
	held := false
	defer func() {
		if held {
			h.ExitInFlight()
		}
	}()
	for {
		q, err := c.Queue(ctx)
		busy := err == nil && q.Busy()
		switch {
		case busy && !held:
			h.EnterInFlight()
			held = true
		case !busy && held:
			h.ExitInFlight()
			held = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
