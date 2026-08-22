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
	"fmt"
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

// Requirements are hardware constraints that must be checked BEFORE renting.
//
// They exist because the alternative is paying to discover them. A live run
// selected a GTX 1060 — the cheapest card whose VRAM held the model — and it
// could never have served with vLLM at any price, because Pascal is below the
// compute capability vLLM supports. VRAM fit answered the wrong question on
// its own.
type Requirements struct {
	// MinComputeCapability is the architecture level times 100: 700 for
	// Volta, 750 Turing, 800 Ampere, 890 Ada. Zero means no constraint.
	MinComputeCapability int

	// Why explains the constraint in the exclusion message, so an operator
	// seeing a cheap card rejected knows it was not arbitrary.
	Why string
}

// Satisfies reports whether hardware meets the requirement.
//
// An offer that does not report its capability is allowed through rather than
// excluded: absence of data is not evidence of incompatibility, and failing
// closed on a missing field would empty the market whenever a provider stopped
// populating it.
func (r Requirements) Satisfies(computeCapability int) (bool, string) {
	if r.MinComputeCapability == 0 || computeCapability == 0 {
		return true, ""
	}
	if computeCapability >= r.MinComputeCapability {
		return true, ""
	}
	return false, fmt.Sprintf("compute capability %.1f below the %.1f %s requires",
		float64(computeCapability)/100, float64(r.MinComputeCapability)/100, r.Why)
}

// CredentialTaker is implemented by runtimes that need a credential to fetch
// weights.
//
// It is a separate interface rather than a field on ModelSpec because a token
// is a secret and ModelSpec is persisted in state (FR-STATE-05). Handing it
// over at launch keeps it out of every snapshot and journal entry that carries
// the spec.
type CredentialTaker interface {
	SetHuggingFaceToken(secret.Secret)
}

// Runtime is an inference engine.
type Runtime interface {
	Kind() core.RuntimeKind

	// Requires reports hardware constraints to apply during selection.
	Requires() Requirements

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
