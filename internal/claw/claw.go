// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package claw is the generic layer over LARRI's rental lifecycle for
// applications that need a GPU.
//
// LARRI is an inference engine first: `larri up` rents hardware, serves a
// model, and publishes a stable local /v1. That is the product, and it is
// unchanged. But the lifecycle underneath it — renting, pinning a host key,
// tunnelling to a loopback bind, supervising on evidence, destroying with
// confirmation — never depended on the payload being an engine, and there is a
// whole class of applications that want exactly that lifecycle.
//
// A *claw* is one such application. This package is where they plug in: a Kind
// knows how to read its own configuration, what hardware that configuration
// implies, what runs on the rented box, and what the operator gets back.
// Everything else — the market, the ranking, the supervision, the teardown — is
// shared and knows nothing about any of them. The daemon imports this package
// and never an implementation of it.
//
// The word is deliberately not "workload", which already means something else
// one layer down: a runtime.Workload is the process that ends up running on the
// rented box. A claw is the job the operator asked for. One produces the other.
//
//	larri claw --type <name> --config job.yml
//	  claw.Kind         reads the config, plans the hardware, collects results
//	  runtime.Workload  the process it stands up on the rented box
//	  daemon            the rental lifecycle, which knows neither
//
// Implementations live in subpackages — internal/claw/<name> — for the same
// reason provider adapters live under internal/provider: the parent defines the
// contract, the children implement it, and the import direction makes a cycle
// impossible.
package claw

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/wire"
)

// Site is where the application itself runs.
//
// This is the distinction that decides almost everything else about a claw:
// what is placed on the rented box, how the operator reaches it, what counts as
// activity, and — the one that matters most — what has to happen in the moment
// before an irreversible destroy.
type Site string

const (
	// SiteRemote: the application runs on the rented box. Whatever it
	// produces exists only there, so it must be collected before teardown or
	// it is gone.
	SiteRemote Site = "remote"

	// SiteLocal: the application runs on the operator's machine and only the
	// inference is rented. Nothing of the operator's lives on the host, so
	// there is nothing to rescue — but their client configuration was
	// changed, and it has to be put back.
	SiteLocal Site = "local"
)

// Valid reports whether s is a site this package knows.
func (s Site) Valid() bool { return s == SiteRemote || s == SiteLocal }

// Options are the settings every claw honours, whatever its type.
//
// Type-specific settings do not appear here and must not: a field per
// application is how a generic command stops being generic. Those live in the
// config file, where the Kind that understands them decodes them.
type Options struct {
	// Criteria are the operator's own hardware floors. A Plan may raise them
	// — the claw knows what it needs — and must never lower them.
	Criteria core.Criteria

	// OutputDir is where a remote claw's results are saved locally. Empty for
	// a local claw, which has nothing to bring back.
	OutputDir string

	// HFToken authenticates weight downloads for gated repositories.
	HFToken secret.Secret

	// SessionHours is how long the operator expects to work. It is a ranking
	// input rather than a limit: whatever the claw must download is billed at
	// the rig's hourly rate, so the cheapest listing is routinely the dearest
	// rig (§4b).
	SessionHours float64
}

// Plan is everything a claw knows before anything is rented.
//
// The whole point of the type is that it is produced without spending. §4a is
// unambiguous: a precondition that can be established without renting must be,
// because the alternative is paying to discover it. A Kind that returns a Plan
// has already read its config, resolved whatever it needs, measured it, and
// turned that into hardware — locally, freely, before any create call.
type Plan struct {
	// Criteria is what to rent: the operator's floors, raised by what the
	// claw needs. See RaiseCriteria.
	Criteria core.Criteria

	// Model describes the payload in the vocabulary the rest of LARRI
	// persists. Rig carries a ModelSpec and the journal records it, so every
	// claw needs one even where "the model" is a set of files.
	Model core.ModelSpec

	// Sizing, when non-nil, is a fixed VRAM requirement the claw computed
	// itself — the sum of a file bundle, say, rather than a transformer's
	// weights and KV cache.
	//
	// Nil is the common case and means the standard path sizes Model from
	// live model facts, re-planning per candidate offer so that shard degree
	// and per-card headroom are accounted for. A claw serving an ordinary
	// model wants nil and should not compute anything.
	Sizing *core.SizingPlan

	// ColdStartBytes is what must be downloaded before the claw can work. It
	// feeds the ranking, which is why a slow link loses to a dearer card over
	// a short session (§4b). Zero ranks on the hourly rate alone.
	ColdStartBytes uint64

	// Summary is what the operator is shown before the confirmation: what was
	// read, what will be fetched, what will be rented.
	Summary []string

	// Caveats are places where this claw's guarantees are weaker than usual.
	// Surfaced at bring-up, because a caveat nobody is shown is not a caveat.
	Caveats []string
}

// Result is what a session produced and what became of it.
type Result struct {
	// Saved and Skipped are what reached local disk; Failed is what did not,
	// keyed by name with the reason. Failures are counted rather than
	// returned as one error because the caller is on its way to a teardown:
	// "four of five saved" is a fact an operator can act on, while an aborted
	// collection that saved nothing is not.
	Saved   []string
	Skipped []string
	Failed  map[string]string
	Bytes   uint64
	Dir     string
}

// Complete reports whether everything the host held is now also local.
func (r *Result) Complete() bool { return r != nil && len(r.Failed) == 0 }

// Count is how many artefacts are held locally.
func (r *Result) Count() int {
	if r == nil {
		return 0
	}
	return len(r.Saved) + len(r.Skipped)
}

// Summary is one line for the operator.
func (r *Result) Summary() string {
	if r == nil {
		return "nothing was collected"
	}
	if r.Count() == 0 && len(r.Failed) == 0 {
		return "no outputs were produced"
	}
	s := fmt.Sprintf("%d saved", len(r.Saved))
	if len(r.Skipped) > 0 {
		s += fmt.Sprintf(", %d already local", len(r.Skipped))
	}
	if len(r.Failed) > 0 {
		s += fmt.Sprintf(", %d FAILED", len(r.Failed))
	}
	return s
}

// Kind is one application LARRI can rent hardware for.
//
// The split between the methods is the point: everything that can happen before
// the money happens in Plan, and everything that must happen before the host is
// destroyed happens in the site-specific half below. What runs in between is
// the shared lifecycle, which never learns which Kind it is carrying.
type Kind interface {
	// Type is the name the operator passes to --type.
	Type() Type

	// Site is where the application runs. It decides what the lifecycle does
	// either side of the rental.
	Site() Site

	// Describe is one line for `larri claw --list`.
	Describe() string

	// Plan reads the configuration and says what to rent. It must not spend,
	// and it must fail here rather than later for anything it can already
	// establish is wrong (§4a).
	Plan(ctx context.Context, cfg *Config, opt Options) (*Plan, error)

	// Server builds the host-side workload the plan implies. For a remote
	// claw that is the application; for a local one it is the inference
	// engine the local application will call.
	Server(p *Plan) runtime.Workload
}

// Remote is a claw whose application runs on the rented box.
type Remote interface {
	Kind

	// Collect retrieves whatever the session produced, before teardown.
	//
	// It runs immediately before an irreversible destroy, so the caller
	// bounds it and it must never be able to prevent one: files left behind
	// are lost once, while a rig left alive bills until somebody notices
	// (§4). Anything it could not bring back belongs in Result.Failed rather
	// than in an error, so the teardown proceeds and still says what was
	// lost.
	Collect(ctx context.Context, sess runtime.Session, dir string, since time.Time) (*Result, error)
}

// Local is a claw whose application runs on the operator's machine and calls
// the rented box for inference.
//
// It returns client writers rather than an Apply/Revert pair of its own,
// because the protocol those follow is already specified and non-negotiable
// (§10.2, FR-WIRE-04/05): detect, back up, apply idempotently, record before
// the change takes effect, revert exactly on down, and verify by probing. A
// claw writing its own wiring would be a weaker duplicate of a contract already
// reasoned through — and what it protects against is a corrupted config file
// belonging to somebody's editor.
type Local interface {
	Kind

	// Clients are the local applications to wire to the endpoint.
	Clients() []wire.ClientWriter
}

// Browser is implemented by a claw the operator opens rather than configures
// into a client.
//
// Two consequences follow from that one fact and neither can be inferred: the
// local listener needs a cookie-shaped credential, since neither a navigation
// nor a WebSocket handshake can carry an Authorization header, and the content
// rendered comes from a host with root (§15.4).
type Browser interface {
	// CountsAsWork decides which proxied requests reset the idle clock.
	//
	// Returning nil counts every non-probe request, which is right for a
	// surface with no browser attached and wrong for one that has: a tab left
	// open polls indefinitely, and an idle timeout counting that would never
	// fire.
	CountsAsWork() func(*http.Request) bool
}

// Holder is implemented by a claw that can hold the idle clock open across work
// producing no requests.
//
// Idle reclamation destroys, and some work is a single short request followed
// by minutes of computation. An idle timer measuring requests cannot tell that
// from an abandoned rig, so the claw is asked.
type Holder interface {
	// HoldWhileBusy blocks until ctx is done, bracketing the in-flight count
	// while the application has outstanding work.
	//
	// A failed check must release the hold rather than extend it: an
	// unreachable application is a rig in trouble, and supervision is better
	// placed to decide about that than a helper whose only move is to keep
	// paying.
	HoldWhileBusy(ctx context.Context, ep LocalEndpoint, h InFlight)
}

// LocalEndpoint is LARRI's own listener, for a claw that needs to reach its
// application through the tunnel.
type LocalEndpoint struct {
	// Addr is host:port of the local listener.
	Addr string

	// Token is the client credential for it. Never the rig's: the proxy
	// strips this one and substitutes its own (FR-SEC-22).
	Token string
}

// InFlight is the part of wire.Activity a claw needs, taken as an interface so
// no implementation depends on the proxy.
type InFlight interface {
	EnterInFlight()
	ExitInFlight()
}

// RaiseCriteria merges what a claw needs into what the operator asked for.
//
// It only ever raises. An operator who asked for 80 GB of VRAM has said
// something about the hardware they want, and a claw needing 12 GB has not
// contradicted them — quietly lowering a floor would rent something cheaper
// than what was asked for, which is the one direction that cannot be undone
// after the fact.
func RaiseCriteria(base, want core.Criteria) core.Criteria {
	out := base
	raise := []struct {
		into *int
		from int
	}{
		{&out.VRAMPerGPUGB, want.VRAMPerGPUGB},
		{&out.VRAMTotalGB, want.VRAMTotalGB},
		{&out.RAMGB, want.RAMGB},
		{&out.DiskGB, want.DiskGB},
		{&out.CPUCores, want.CPUCores},
		{&out.GPUCount, want.GPUCount},
	}
	for _, r := range raise {
		if r.from > *r.into {
			*r.into = r.from
		}
	}
	if want.MinNetMbps > out.MinNetMbps {
		out.MinNetMbps = want.MinNetMbps
	}
	return out
}
