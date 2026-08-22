// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package fake is a Provider that simulates the ways a real marketplace
// misbehaves.
//
// It is a first-class component, not test scaffolding. Most of LARRI's
// cost-safety requirements are only testable because it exists: a fake that
// merely succeeds would let every one of them pass while the real failure
// modes went unexercised.
//
// It models the disappearance taxonomy of LARRI-DES-001 §12.1 — including the
// two that cost money and look like success: a stop that is not a destroy, and
// a List that omits non-running resources.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/provider"
)

// Behaviour configures how the fake misbehaves.
type Behaviour struct {
	// CreateTimesOutButSucceeds reproduces R-07: the call returns an error,
	// the instance exists anyway. A caller that blind-retries ends up paying
	// for two.
	CreateTimesOutButSucceeds bool

	// DestroyOnlyStops reproduces a provider whose delete stops the container
	// rather than removing it. Storage keeps billing, and only a check for
	// absence catches it (FR-DEL-03).
	DestroyOnlyStops bool

	// ListOmitsStopped reproduces an adapter or API that returns only running
	// instances, which would make a storage-billing container read as gone.
	ListOmitsStopped bool

	// Unreachable makes every call fail with a transport error. The rig's
	// state is then unknown, and nothing may be concluded from it
	// (FR-SUP-11).
	Unreachable bool

	// TransientFailures is the number of times each mutating call fails with
	// a retryable error before succeeding.
	TransientFailures int
}

// Provider is a fake marketplace.
type Provider struct {
	mu        sync.Mutex
	name      string
	behaviour Behaviour
	offers    []core.Offer
	instances map[string]*core.Instance
	nextID    int
	now       func() time.Time
	transient map[string]int

	// Calls records every method invoked, so tests can assert on sequence —
	// notably that a reconcile happened before a retry.
	Calls []string
}

var _ provider.Provider = (*Provider)(nil)

// New builds a fake with the given offers.
func New(name string, offers []core.Offer, b Behaviour) *Provider {
	return &Provider{
		name:      name,
		behaviour: b,
		offers:    offers,
		instances: make(map[string]*core.Instance),
		now:       time.Now,
		transient: make(map[string]int),
	}
}

// SetClock replaces the time source, so cost accrual is testable without
// sleeping.
func (p *Provider) SetClock(f func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = f
}

func (p *Provider) Name() string { return p.name }

func (p *Provider) record(op string) { p.Calls = append(p.Calls, op) }

func (p *Provider) gate(op string) error {
	if p.behaviour.Unreachable {
		return errs.Newf(errs.ClassProviderTransient, p.name+"."+op,
			"provider unreachable")
	}
	if n := p.behaviour.TransientFailures; n > 0 && p.transient[op] < n {
		p.transient[op]++
		return errs.Newf(errs.ClassProviderTransient, p.name+"."+op,
			"rate limited (attempt %d of %d)", p.transient[op], n)
	}
	return nil
}

// Search returns offers satisfying the criteria.
func (p *Provider) Search(ctx context.Context, c core.Criteria) ([]core.Offer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("Search")
	if err := p.gate("Search"); err != nil {
		return nil, err
	}
	var out []core.Offer
	for _, o := range p.offers {
		if !c.Interruptible.Permits(o.Interruptible) {
			continue
		}
		if c.MaxPriceHr > 0 && o.PriceHr > c.MaxPriceHr {
			continue
		}
		if c.VRAMPerGPUGB > 0 && o.VRAMPerGPUGB < c.VRAMPerGPUGB {
			continue
		}
		if c.MinReliability > 0 && o.Reliability < c.MinReliability {
			continue
		}
		if c.CertifiedOnly && !o.Certified {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}

// Create purchases an offer.
func (p *Provider) Create(ctx context.Context, o core.Offer, spec provider.CreateSpec) (*core.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("Create")
	if err := p.gate("Create"); err != nil {
		return nil, err
	}
	p.nextID++
	id := fmt.Sprintf("%s-%d", p.name, p.nextID)
	inst := &core.Instance{
		Provider:   p.name,
		InstanceID: id,
		OfferID:    o.OfferID,
		PriceHr:    o.PriceHr,
		StorageHr:  0.01,
		Running:    true,
		SSHHost:    id + ".fake.invalid",
		SSHPort:    22,
		CreatedAt:  p.now(),
		// Normalised to the bare rig ID, matching the real adapters: the
		// prefix is provider vocabulary and dies at this boundary, so
		// reconciliation can compare a label to a rig ID it already holds.
		Labels: labelsFrom(spec.Label),
	}
	p.instances[id] = inst

	if p.behaviour.CreateTimesOutButSucceeds {
		// The instance exists. The caller does not know that.
		return nil, errs.Newf(errs.ClassProviderUnknownOutcome, p.name+".Create",
			"timeout awaiting create response")
	}
	cp := *inst
	return &cp, nil
}

// Get returns one instance, running or not.
func (p *Provider) Get(ctx context.Context, id string) (*core.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("Get")
	if err := p.gate("Get"); err != nil {
		return nil, err
	}
	inst, ok := p.instances[id]
	if !ok {
		return nil, nil // absent: the only proof of destruction
	}
	cp := *inst
	return &cp, nil
}

// List returns every instance this provider holds.
func (p *Provider) List(ctx context.Context) ([]core.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("List")
	if err := p.gate("List"); err != nil {
		return nil, err
	}
	var out []core.Instance
	for _, inst := range p.instances {
		if p.behaviour.ListOmitsStopped && !inst.Running {
			continue // the bug being simulated
		}
		out = append(out, *inst)
	}
	return out, nil
}

// Destroy removes an instance, or — if configured — merely stops it.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.record("Destroy")
	if err := p.gate("Destroy"); err != nil {
		return err
	}
	inst, ok := p.instances[id]
	if !ok {
		return nil // idempotent
	}
	if p.behaviour.DestroyOnlyStops {
		inst.Running = false // still exists, still billing storage
		return nil           // and the API reports success
	}
	delete(p.instances, id)
	return nil
}

// Stop simulates the provider stopping an instance out from under LARRI —
// outbid, host-stopped, or balance exhausted. The container still exists.
func (p *Provider) Stop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inst, ok := p.instances[id]; ok {
		inst.Running = false
	}
}

// Resume simulates a stopped interruptible coming back on its own when its bid
// clears again. If LARRI provisioned a replacement meanwhile, there are now two
// billing instances (R-14).
func (p *Provider) Resume(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inst, ok := p.instances[id]; ok {
		inst.Running = true
	}
}

// Vanish simulates an instance destroyed outside LARRI.
func (p *Provider) Vanish(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.instances, id)
}

// SetUnreachable toggles provider-wide failure.
func (p *Provider) SetUnreachable(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.behaviour.Unreachable = v
}

// labelsFrom normalises a provider-side marker the way real adapters do: the
// parsed rig ID for comparison, and the raw marker for a future reader.
func labelsFrom(raw string) map[string]string {
	l, ok := core.DecodeLabel(raw)
	if !ok {
		return map[string]string{core.LabelKey: raw}
	}
	return map[string]string{core.LabelKey: l.RigID, core.LabelRawKey: raw}
}

// Count returns how many instances exist, running or not. Tests assert on this
// to prove nothing was left behind.
func (p *Provider) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.instances)
}
