// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package fake is a Runtime that boots, serves, and fails on demand.
//
// Like the fake provider, it exists so the lifecycle is exercisable with zero
// spend (NFR-09) and so the failure paths that matter — a slow weight
// download, a runtime that starts but never completes, an OOM at load — are
// reachable in a unit test.
package fake

import (
	"bytes"
	"context"
	"io"
	"sync"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

// Behaviour configures how the fake runtime misbehaves.
type Behaviour struct {
	// BootstrapFails is host-attributable: the next offer may work.
	BootstrapFails bool
	// OOMAtLoad is model-attributable: the next host fails identically, so
	// this must not trigger a fallback (FR-PROV-05).
	OOMAtLoad bool
	// NeverReady starts the server but never completes a request — the case
	// a TCP-connect readiness check would wrongly call healthy.
	NeverReady bool
	// WeightBytes is the simulated download size, reported through Progress
	// and answered to WeightSizer — the same number in both, because a
	// runtime that knows what it is about to fetch knows what it will load.
	// Zero means it cannot say, which is the case that falls back to the
	// estimate.
	WeightBytes int64
	// BindHost overrides the bind address, to prove a non-loopback bind is
	// rejected rather than merely warned about.
	BindHost string
	// ExitsAfterLaunch simulates a runtime that starts, writes something, and
	// dies — the case where waiting out a stall timeout bills for an outcome
	// that has already been decided.
	ExitsAfterLaunch bool
	// TensorParallel makes this engine split a model the way vLLM does, so
	// selection has to ask whether a degree divides the model's heads rather
	// than assuming every card on the host is usable.
	TensorParallel bool
}

// Runtime is a fake inference engine.
type Runtime struct {
	mu        sync.Mutex
	behaviour Behaviour
	launched  bool
	stopped   bool
}

var _ runtime.Runtime = (*Runtime)(nil)

// New builds a fake runtime.
func New(b Behaviour) *Runtime { return &Runtime{behaviour: b} }

// defaultDownloadBytes is what an unconfigured fake pretends to download:
// ~19.1 GB, a realistic q4 30B.
const defaultDownloadBytes = 19_100_000_000

// downloadBytes is the size Progress reports.
//
// The default is applied here rather than stored in Behaviour, so that an
// unset field stays unset for everything else. It did not, once: filling it in
// at construction made every fake runtime answer WeightSizer with 19.1 GB, and
// a 400B model that no card can hold sized as a 19 GB one and passed the fit
// check on a single 24 GB card.
func (r *Runtime) downloadBytes() int64 {
	if r.behaviour.WeightBytes <= 0 {
		return defaultDownloadBytes
	}
	return r.behaviour.WeightBytes
}

func (r *Runtime) Kind() core.RuntimeKind { return "fake" }

func (r *Runtime) Image(core.ModelSpec, core.SizingPlan) string {
	return "ghcr.io/sovrenix/larri-fake@sha256:" + "0"
}

// Bootstrap reports progress in a few steps so callers that render a bar are
// exercised, then fails if configured to.
func (r *Runtime) Bootstrap(ctx context.Context, _ runtime.Session, spec core.ModelSpec, _ core.SizingPlan, progress chan<- runtime.Progress) error {
	send := func(p runtime.Progress) {
		if progress == nil {
			return
		}
		select {
		case progress <- p:
		case <-ctx.Done():
		}
	}
	send(runtime.Progress{Phase: runtime.PhaseImagePull, Percent: 100})

	if r.behaviour.BootstrapFails {
		return errs.Newf(errs.ClassHostFailure, "fake.Bootstrap",
			"image pull failed on this host")
	}
	for _, pct := range []float64{25, 50, 75, 100} {
		if err := ctx.Err(); err != nil {
			return err
		}
		send(runtime.Progress{
			Phase:      runtime.PhaseWeightsDownload,
			Percent:    pct,
			BytesDone:  int64(float64(r.downloadBytes()) * pct / 100),
			BytesTotal: r.downloadBytes(),
		})
	}
	return nil
}

// Launch starts the fake server.
func (r *Runtime) Launch(ctx context.Context, _ runtime.Session, spec core.ModelSpec, _ core.SizingPlan) (runtime.Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.behaviour.OOMAtLoad {
		// The class carries the policy — ClassModelFailure means "do not retry
		// on another host". The message therefore states only the fact.
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "fake.Launch",
			"cuda out of memory loading %s", spec.Ref)
	}
	host := runtime.Loopback
	if r.behaviour.BindHost != "" {
		host = r.behaviour.BindHost
	}
	key, err := secret.Generate(32)
	if err != nil {
		return runtime.Endpoint{}, err
	}
	ep := runtime.Endpoint{Host: host, Port: 8000, Model: spec.ServedName, Key: key}

	// FR-SEC-08: a non-loopback bind is rejected at launch, not warned about.
	if !ep.Valid() {
		return runtime.Endpoint{}, errs.Newf(errs.ClassModelFailure, "fake.Launch",
			"invalid bind address %s: loopback only", host)
	}
	r.launched = true
	return ep, nil
}

// Ready performs the completion round-trip that READY actually means.
func (r *Runtime) Ready(ctx context.Context, ep runtime.Endpoint, _ core.ModelSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.launched {
		return errs.Newf(errs.ClassHostFailure, "fake.Ready", "runtime not launched")
	}
	if r.behaviour.NeverReady {
		// The port is open. A TCP check would pass. A completion does not.
		return errs.Newf(errs.ClassHostFailure, "fake.Ready",
			"no completion returned within deadline")
	}
	return nil
}

func (r *Runtime) Logs(context.Context, runtime.Session, int) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("fake runtime log\n"))), nil
}

func (r *Runtime) Stop(context.Context, runtime.Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.launched, r.stopped = false, true
	return nil
}

// Stopped reports whether Stop was called, so teardown ordering is assertable.
func (r *Runtime) Stopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

// Session is a fake Session that records the commands it was asked to run.
type Session struct {
	mu   sync.Mutex
	Cmds []string
}

var _ runtime.Session = (*Session)(nil)

func (s *Session) Run(_ context.Context, cmd string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Cmds = append(s.Cmds, cmd)
	return nil, nil
}

func (s *Session) Dial(context.Context, int) (io.ReadWriteCloser, error) { return nil, nil }
func (s *Session) Close() error                                          { return nil }

// Requires reports no hardware floor by default.
// WeightBytes implements runtime.WeightSizer.
func (r *Runtime) WeightBytes() uint64 {
	if r.behaviour.WeightBytes <= 0 {
		return 0
	}
	return uint64(r.behaviour.WeightBytes)
}

func (r *Runtime) Requires() runtime.Requirements {
	return runtime.Requirements{TensorParallel: r.behaviour.TensorParallel, Why: "the engine"}
}

// Alive reports whether the fake server process would still exist.
func (r *Runtime) Alive(context.Context, runtime.Session) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Alive unless the test asks otherwise. Tests that drive waitReady
	// directly never call Launch, and a fake that called those runs dead
	// would be modelling the harness rather than a runtime.
	return !r.behaviour.ExitsAfterLaunch, nil
}

var _ runtime.LivenessChecker = (*Runtime)(nil)

// LogPath lets the fake stand in for a real runtime in the supervisor's
// progress logic, which asks the engine where it writes.
func (r *Runtime) LogPath() string { return "/var/log/larri-fake.log" }

var _ runtime.LogWriter = (*Runtime)(nil)
