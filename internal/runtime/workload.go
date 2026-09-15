// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"context"
	"io"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
)

// Protocol is what a workload's endpoint speaks once it is up.
//
// It exists because LARRI's second abstraction was written as "an inference
// engine" and the lifecycle underneath it never was. Renting, pinning a host
// key, tunnelling to a loopback bind, supervising on evidence, and destroying
// with confirmation are all indifferent to whether the thing on the far end
// completes tokens or renders images — and P2's "/v1 is the contract" is a
// statement about inference clients, not about what may be rented.
//
// So the payload declares its protocol rather than having one assumed of it.
// A Runtime says ProtocolOpenAI and everything above it may issue a chat
// completion; an Application says something else and nothing above it may.
// The alternative was a ComfyUI adapter whose Ready() returned an image while
// its interface promised a completion, which is a lie the type system would
// have helped nobody catch.
type Protocol string

const (
	// ProtocolOpenAI is the OpenAI-compatible /v1 surface of P2. Every
	// Runtime serves it, and it is what the IDE and chat wiring depend on.
	ProtocolOpenAI Protocol = "openai-v1"

	// ProtocolComfyUI is ComfyUI's HTTP API plus its bundled web frontend:
	// POST /prompt to enqueue, /history to poll, /view to fetch an output,
	// and a WebSocket at /ws carrying execution progress.
	ProtocolComfyUI Protocol = "comfyui"
)

// Browser reports whether a protocol is meant to be opened in one.
//
// The distinction is load-bearing rather than cosmetic. A protocol an
// operator points a browser at cannot authenticate with a bearer header —
// navigation and WebSocket handshakes carry no Authorization — so the local
// listener needs a cookie-shaped credential for it, and the content it
// renders comes from a host with root (§15.4). Both consequences follow from
// this one bit, so it is asked rather than inferred at each site.
func (p Protocol) Browser() bool { return p == ProtocolComfyUI }

// Workload is anything LARRI can stand up on a rented host and hold open.
//
// This is the second abstraction of P1, widened from Runtime. The five things
// an implementation owns are unchanged in kind — how the payload arrives, how
// VRAM fit is computed, what "ready" means, what it binds, and how it stops —
// and nothing above this layer may branch on which implementation is behind
// the endpoint.
//
// Ready is the method that carries the widening. It has always meant "a real
// round-trip, not a TCP connect" (NFR-05), and that reading survives intact:
// for a Runtime the round-trip is a completion, for ComfyUI it is a rendered
// image. What changes is only that the caller can no longer assume which.
type Workload interface {
	Kind() core.RuntimeKind

	// Protocol reports what the endpoint speaks, so callers that need /v1
	// can require it rather than discover its absence at runtime.
	Protocol() Protocol

	// Requires reports hardware constraints to apply during selection.
	Requires() Requirements

	// Image returns the container image for this spec and plan.
	Image(spec core.ModelSpec, plan core.SizingPlan) string

	// Bootstrap acquires the image and payload on the host.
	Bootstrap(ctx context.Context, sess Session, spec core.ModelSpec, plan core.SizingPlan, progress chan<- Progress) error

	// Launch starts the server and returns its endpoint, which must be
	// loopback-bound.
	Launch(ctx context.Context, sess Session, spec core.ModelSpec, plan core.SizingPlan) (Endpoint, error)

	// Ready performs a real round-trip. A TCP connect or a 200 on /health is
	// necessary but not sufficient (NFR-05).
	Ready(ctx context.Context, ep Endpoint, spec core.ModelSpec) error

	// Logs streams the payload's logs for diagnosis.
	Logs(ctx context.Context, sess Session, tail int) (io.ReadCloser, error)

	// Stop halts the payload.
	Stop(ctx context.Context, sess Session) error
}

// FailureClassifier is implemented by workloads that can read their own log
// and say whether the failure belongs to the host or to the configuration.
//
// It exists because waitReady cannot tell the difference and had been guessing
// the expensive way. Every readiness failure was ClassHostFailure, which means
// "try another machine" — correct for a host that never booted, and wrong for
// a payload that died on an import error, where the next machine runs the same
// image and dies identically. FR-PROV-05 says exactly that, and a live ComfyUI
// run paid for it three times over: a torch too old for the application,
// rented, installed and fed six gigabytes of weights on three separate hosts
// before giving up.
//
// The workload is the only layer that can read its own traceback, so it is the
// layer that decides. Returning ClassUnknown means "no opinion", and the
// caller keeps its host-failure default.
type FailureClassifier interface {
	ClassifyFailure(log string) errs.Class
}

// ClassifyFailure asks a workload to classify its own death, falling back to
// the host-failure default for one that has no opinion.
func ClassifyFailure(w Workload, log string) errs.Class {
	c, ok := w.(FailureClassifier)
	if !ok {
		return errs.ClassHostFailure
	}
	if cl := c.ClassifyFailure(log); cl != errs.ClassUnknown {
		return cl
	}
	return errs.ClassHostFailure
}

// RequireOpenAI reports whether a workload serves the /v1 contract, so a
// caller that is about to issue a chat completion can say so in one place
// rather than comparing protocols at each site.
func RequireOpenAI(w Workload) bool { return w != nil && w.Protocol() == ProtocolOpenAI }
