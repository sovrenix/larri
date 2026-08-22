// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package provider is one of LARRI's two abstractions (P1).
//
// The interface is deliberately narrow, and the reason is not aesthetic: it is
// narrow enough to fake, and a fake is what allows the full lifecycle to be
// exercised with zero spend (NFR-09). No test may issue a real create.
package provider

import (
	"context"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/secret"
)

// Provider is a normalised rental marketplace.
type Provider interface {
	Name() string

	// Search returns offers matching the criteria, normalised.
	Search(ctx context.Context, c core.Criteria) ([]core.Offer, error)

	// Create purchases an offer. The instance is stamped with spec.Label
	// before this call is made, so a crash mid-create leaves an attributable
	// resource behind (FR-STATE-04).
	Create(ctx context.Context, o core.Offer, spec CreateSpec) (*core.Instance, error)

	// Get returns one instance. It must report instances that exist but are
	// not running, since those still bill for storage (§12.4).
	Get(ctx context.Context, instanceID string) (*core.Instance, error)

	// List returns ALL instances at this provider, not only LARRI's and not
	// only running ones.
	//
	// Both halves matter. Returning everything lets the reconciler tell a
	// LARRI orphan from the operator's own instance. Returning non-running
	// resources is what makes STOPPED detectable at all — a List that omitted
	// them would report a storage-billing container as absent, and teardown
	// would journal it DESTROYED while it billed on (R-13).
	List(ctx context.Context) ([]core.Instance, error)

	// Destroy removes an instance. It is idempotent, and a nil error is a
	// claim rather than proof: absence from List is the evidence (FR-DEL-03).
	Destroy(ctx context.Context, instanceID string) error
}

// CreateSpec is what to make of a purchased offer.
type CreateSpec struct {
	Image        string
	SSHPublicKey string
	DiskGB       int
	Env          map[string]secret.Secret
	Ports        []int  // SSH only, unless direct port-mapping was enabled (FR-SEC-15)
	Label        string // "larri:<rigID>", stamped before the call
	OnStart      string
}

// Registry holds the enabled providers.
type Registry struct{ byName map[string]Provider }

// NewRegistry builds a registry from the given providers.
func NewRegistry(ps ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(ps))}
	for _, p := range ps {
		r.byName[p.Name()] = p
	}
	return r
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// All returns every registered provider, in a deterministic order so that
// search results and reconciliation reports do not reshuffle between runs.
func (r *Registry) All() []Provider {
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sortStrings(names)
	out := make([]Provider, 0, len(names))
	for _, n := range names {
		out = append(out, r.byName[n])
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
