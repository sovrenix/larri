// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package runtime is LARRI's second abstraction (P1).
//
// Nothing above this layer knows whether vLLM, llama.cpp, or Ollama is behind
// the endpoint (P2). Runtimes differ in exactly four places, and all four live
// inside an implementation: how weights are acquired, how VRAM fit is
// computed, what "ready" means, and whether tool calling is enabled at launch.
package runtime

import (
	"context"
	"io"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/secret"
)

// Endpoint is where a runtime serves, on the rented host.
//
// Host is always loopback. FR-SEC-08: the bind address is computed here and is
// not configurable — there is no flag, no config key, and a non-loopback value
// is rejected at launch rather than warned about. An inference server on a
// routable port is unauthenticated access to hardware the operator pays for,
// on a public IP shared with other tenants.
type Endpoint struct {
	Host  string
	Port  int
	Model string        // the stable served name, not the upstream ref
	Key   secret.Secret // the rig token; never a client's token (FR-SEC-22)
}

// Loopback is the only address a runtime may bind.
const Loopback = "127.0.0.1"

// Valid reports whether the endpoint binds loopback as required.
func (e Endpoint) Valid() bool { return e.Host == Loopback || e.Host == "localhost" }

// Phase names a stage of bootstrap, for progress reporting.
type Phase string

const (
	PhaseImagePull       Phase = "image.pull"
	PhaseWeightsDownload Phase = "weights.download"
	PhaseLaunch          Phase = "launch"
	PhaseReady           Phase = "ready"
)

// Progress carries bootstrap progress so a multi-GB download does not look
// like a hang (FR-RT-06). Bytes are reported because the operator is paying
// for the time they take.
type Progress struct {
	Phase       Phase
	Percent     float64
	BytesDone   int64
	BytesTotal  int64
	BytesPerSec float64
	Message     string
}

// Session is an authenticated channel to the rented host.
//
// It is satisfied by internal/sshx. Nothing here shells out: SSH is spoken
// in-process, so agent forwarding and friends are not implemented rather than
// disabled by a flag someone could re-enable in ~/.ssh/config (§15.5.2).
type Session interface {
	// Run executes a command and returns its combined output.
	Run(ctx context.Context, cmd string) ([]byte, error)
	// Dial opens a channel to a port on the host's loopback interface.
	Dial(ctx context.Context, port int) (io.ReadWriteCloser, error)
	// Close releases the session.
	Close() error
}

// Runtime is an inference engine.
type Runtime interface {
	Kind() core.RuntimeKind

	// Image returns the container image for this spec and plan. M1 uses a
	// stock image; the pre-baked, digest-pinned matrix (§6.5) follows.
	Image(spec core.ModelSpec, plan core.SizingPlan) string

	// Bootstrap acquires the image and weights on the host.
	Bootstrap(ctx context.Context, sess Session, spec core.ModelSpec, plan core.SizingPlan, progress chan<- Progress) error

	// Launch starts the server and returns its endpoint, which must be
	// loopback-bound.
	Launch(ctx context.Context, sess Session, spec core.ModelSpec, plan core.SizingPlan) (Endpoint, error)

	// Ready performs a real completion round-trip. A TCP connect or a 200 on
	// /health is necessary but not sufficient: READY means a completion has
	// come back (NFR-05).
	Ready(ctx context.Context, ep Endpoint, spec core.ModelSpec) error

	// Logs streams runtime logs for diagnosis.
	Logs(ctx context.Context, sess Session, tail int) (io.ReadCloser, error)

	// Stop halts the runtime.
	Stop(ctx context.Context, sess Session) error
}
