// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package fake is a claw that plans, serves and collects on demand.
//
// It exists so both sites are exercisable with zero spend (NFR-09), and so the
// paths that only matter when something goes wrong — a plan that refuses before
// the money, a collection that cannot reach the host, a revert that fails — are
// reachable in a unit test rather than only on a rented box.
//
// Two types rather than one, because the site is expressed in the method set:
// a Remote has Collect and a Local has Clients, and a single type carrying both
// would satisfy both interfaces at once and make the daemon's choice ambiguous.
package fake

import (
	"context"
	"sync"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/wire"
)

// Behaviour configures how the fake claw behaves and misbehaves.
type Behaviour struct {
	// PlanErr refuses before anything is rented, which is the §4a path.
	PlanErr error

	// Criteria is what the claw says it needs. Merged over the operator's
	// floors by claw.RaiseCriteria, never under them.
	Criteria core.Criteria

	// Sizing, when set, is a fixed VRAM requirement — the measured-bundle
	// case. Nil leaves the standard model-sizing path in charge.
	Sizing *core.SizingPlan

	// ColdStartBytes is what must be downloaded before the claw can work.
	ColdStartBytes uint64

	// Model names the payload for the journal.
	Model core.ModelSpec

	// Workload is what runs on the rented box. Nil builds a fake runtime.
	Workload runtime.Workload

	// CollectErr fails a remote claw's collection, which must still not
	// prevent the destroy.
	CollectErr error

	// Collected is what a remote claw brings back. Nil builds an empty
	// result.
	Collected *claw.Result

	// Clients are the local applications a local claw wires.
	Clients []wire.ClientWriter

	// Caveats are surfaced at bring-up.
	Caveats []string
}

// calls records what the lifecycle asked of a claw, so a test can assert the
// order rather than only the outcome.
type calls struct {
	mu        sync.Mutex
	Planned   int
	Served    int
	Collected int
	Clients   int
	Order     []string
}

func (c *calls) note(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Order = append(c.Order, what)
}

// Steps returns the sequence of methods the lifecycle called.
func (c *calls) Steps() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.Order...)
}

// ---- remote -----------------------------------------------------------

// RemoteKind is a claw whose application runs on the rented box.
type RemoteKind struct {
	calls
	b  Behaviour
	tp claw.Type
}

// NewRemote builds a remote claw under the given type name.
func NewRemote(t claw.Type, b Behaviour) *RemoteKind {
	return &RemoteKind{b: b, tp: t}
}

func (k *RemoteKind) Type() claw.Type  { return k.tp }
func (k *RemoteKind) Site() claw.Site  { return claw.SiteRemote }
func (k *RemoteKind) Describe() string { return "a fake remote claw" }

func (k *RemoteKind) Plan(_ context.Context, _ *claw.Config, opt claw.Options) (*claw.Plan, error) {
	k.calls.mu.Lock()
	k.Planned++
	k.calls.mu.Unlock()
	k.note("plan")
	if k.b.PlanErr != nil {
		return nil, k.b.PlanErr
	}
	return planFrom(k.b, opt), nil
}

func (k *RemoteKind) Server(*claw.Plan) runtime.Workload {
	k.calls.mu.Lock()
	k.Served++
	k.calls.mu.Unlock()
	k.note("server")
	return workloadFrom(k.b)
}

func (k *RemoteKind) Collect(_ context.Context, _ runtime.Session, dir string, _ time.Time) (*claw.Result, error) {
	k.calls.mu.Lock()
	k.calls.Collected++
	k.calls.mu.Unlock()
	k.note("collect")
	if k.b.CollectErr != nil {
		return nil, k.b.CollectErr
	}
	if k.b.Collected != nil {
		r := *k.b.Collected
		r.Dir = dir
		return &r, nil
	}
	return &claw.Result{Failed: map[string]string{}, Dir: dir}, nil
}

var _ claw.Remote = (*RemoteKind)(nil)

// ---- local ------------------------------------------------------------

// LocalKind is a claw whose application runs on the operator's machine.
type LocalKind struct {
	calls
	b  Behaviour
	tp claw.Type
}

// NewLocal builds a local claw under the given type name.
func NewLocal(t claw.Type, b Behaviour) *LocalKind {
	return &LocalKind{b: b, tp: t}
}

func (k *LocalKind) Type() claw.Type  { return k.tp }
func (k *LocalKind) Site() claw.Site  { return claw.SiteLocal }
func (k *LocalKind) Describe() string { return "a fake local claw" }

func (k *LocalKind) Plan(_ context.Context, _ *claw.Config, opt claw.Options) (*claw.Plan, error) {
	k.calls.mu.Lock()
	k.Planned++
	k.calls.mu.Unlock()
	k.note("plan")
	if k.b.PlanErr != nil {
		return nil, k.b.PlanErr
	}
	return planFrom(k.b, opt), nil
}

func (k *LocalKind) Server(*claw.Plan) runtime.Workload {
	k.calls.mu.Lock()
	k.Served++
	k.calls.mu.Unlock()
	k.note("server")
	return workloadFrom(k.b)
}

func (k *LocalKind) Clients() []wire.ClientWriter {
	k.calls.mu.Lock()
	k.calls.Clients++
	k.calls.mu.Unlock()
	k.note("clients")
	return k.b.Clients
}

var _ claw.Local = (*LocalKind)(nil)

// ---- shared -----------------------------------------------------------

func planFrom(b Behaviour, opt claw.Options) *claw.Plan {
	model := b.Model
	if model.ServedName == "" {
		model.ServedName = "fake"
	}
	if model.Ref == "" {
		model.Ref = "fake/claw"
	}
	return &claw.Plan{
		Criteria:       claw.RaiseCriteria(opt.Criteria, b.Criteria),
		Model:          model,
		Sizing:         b.Sizing,
		ColdStartBytes: b.ColdStartBytes,
		Summary:        []string{"a fake claw, planned without spending"},
		Caveats:        b.Caveats,
	}
}

func workloadFrom(b Behaviour) runtime.Workload {
	if b.Workload != nil {
		return b.Workload
	}
	return rfake.New(rfake.Behaviour{})
}

// Client is a wire.ClientWriter that records what was done to it, for the
// local site's revert-before-destroy path.
type Client struct {
	ClientName string
	ClientTier wire.Tier
	Present    bool
	ApplyErr   error
	RevertErr  error

	mu       sync.Mutex
	Applied  int
	Reverted int
}

// NewClient builds a tier-A client that is present and cooperative.
func NewClient(name string) *Client {
	return &Client{ClientName: name, ClientTier: wire.TierFile, Present: true}
}

func (c *Client) Name() string          { return c.ClientName }
func (c *Client) Tier() wire.Tier       { return c.ClientTier }
func (c *Client) Detect() (bool, error) { return c.Present, nil }

func (c *Client) Apply(wire.Endpoint) (core.WiringRecord, error) {
	c.mu.Lock()
	c.Applied++
	c.mu.Unlock()
	if c.ApplyErr != nil {
		return core.WiringRecord{}, c.ApplyErr
	}
	return core.WiringRecord{
		Path:       "/fake/" + c.ClientName + ".json",
		BackupPath: "/fake/" + c.ClientName + ".json.bak",
	}, nil
}

func (c *Client) Revert(core.WiringRecord) error {
	c.mu.Lock()
	c.Reverted++
	c.mu.Unlock()
	return c.RevertErr
}

// Counts reports how often this client was wired and unwired.
func (c *Client) Counts() (applied, reverted int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Applied, c.Reverted
}

var _ wire.ClientWriter = (*Client)(nil)
