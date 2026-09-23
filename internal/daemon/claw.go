// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/wire"
)

// ClawRequest is an application session on rented hardware.
//
// The Plan is produced by the Kind before this is called and before anything is
// spent, so by the time the daemon sees a request the hardware question is
// already answered (§4a). That ordering is what lets `--dry-run` print the
// whole report without a create call.
type ClawRequest struct {
	Kind claw.Kind
	Plan *claw.Plan

	// LocalPort is the fixed loopback port the session is published on. Zero
	// lets the kernel choose, which is only useful in tests: P3 depends on
	// this being stable across the rig's life.
	LocalPort int

	HFToken secret.Secret

	// OutputDir is where a remote claw's results are saved. Unused by a local
	// claw, which produces nothing on the host.
	OutputDir string

	Confirm func(offer core.Offer, plan core.SizingPlan) bool
}

// ClawSession is a claw held open for an operator to use.
type ClawSession struct {
	Live *Live
	Kind claw.Kind
	Plan *claw.Plan

	// URL is the one-time link that logs a browser in, for a claw that is
	// opened rather than configured. It carries an exchange token that is
	// spent on first use, so it is printed once, never persisted, and worth
	// nothing to anyone who reads it out of a scrollback afterwards.
	//
	// Ask NewSessionURL for another when a second browser needs one.
	URL string

	// OutputDir is where a remote claw's results are saved locally.
	OutputDir string

	// StartedAt bounds what a teardown collects, so a rig adopted from an
	// earlier session does not re-download what is already held.
	StartedAt time.Time

	// Wiring is what a local claw changed on this machine, kept so it can be
	// put back exactly (§10.2).
	Wiring []core.WiringRecord

	clients map[string]wire.ClientWriter
	hold    context.CancelFunc
}

// ClawUp rents hardware for a claw and holds it open.
//
// Everything expensive has already been decided: the Kind read its config,
// resolved whatever it needs, and turned that into criteria and a size, all
// locally and for nothing. What happens here is the part that spends, and it is
// the same sequence every rig goes through — the daemon never learns which
// application it is carrying.
func (o *Orchestrator) ClawUp(ctx context.Context, req ClawRequest) (*ClawSession, error) {
	if req.Kind == nil || req.Plan == nil {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.ClawUp", "no claw to run")
	}
	if req.Kind.Site() == claw.SiteRemote && req.OutputDir == "" {
		// A remote claw produces things that exist only on the host. Renting
		// one with nowhere to put them is a session whose results are lost by
		// construction.
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.ClawUp",
			"no local output directory for a remote claw")
	}

	o.Runtime = req.Kind.Server(req.Plan)
	if o.Runtime == nil {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.ClawUp",
			"claw %s built no workload", req.Kind.Type())
	}

	// A claw that measured its own requirement supplies it; one serving an
	// ordinary model leaves this nil and the standard path sizes the model
	// from live facts, per candidate offer.
	if p := req.Plan.Sizing; p != nil {
		fixed := *p
		o.Planner = func(context.Context, UpRequest) (core.SizingPlan, error) {
			return fixed, nil
		}
		if req.Plan.ColdStartBytes > 0 {
			o.ColdStart = req.Plan.ColdStartBytes
		}
	}

	for _, line := range req.Plan.Summary {
		o.emit("claw", "%s", line)
	}
	for _, c := range req.Plan.Caveats {
		o.warn("claw", "%s", c)
	}

	started := time.Now()
	live, err := o.UpAndServe(ctx, UpRequest{
		Criteria:  req.Plan.Criteria,
		Model:     req.Plan.Model,
		DiskGB:    req.Plan.Criteria.DiskGB,
		HFToken:   req.HFToken,
		LocalPort: req.LocalPort,
		Confirm:   req.Confirm,
		ClawSite:  string(req.Kind.Site()),
	})
	if err != nil {
		return nil, err
	}

	s := &ClawSession{
		Live: live, Kind: req.Kind, Plan: req.Plan,
		OutputDir: req.OutputDir, StartedAt: started,
	}
	if err := o.attachClaw(s); err != nil {
		// Destroyed, not closed. Live.Close releases the forward and the
		// hold and never the machine, so this path returned an error while
		// leaving a rented GPU billing with nothing supervising it and no
		// holder to reclaim it — the §4 failure the whole teardown protocol
		// exists to prevent. The hold is kept through the destroy and given
		// up after it, as every other teardown here does.
		o.teardownAfterFailure(live.Rig, core.ReasonBootstrapFailed, err)
		_ = live.Close()
		return nil, err
	}
	return s, nil
}

// attachClaw does whatever the site needs once the rig is serving.
func (o *Orchestrator) attachClaw(s *ClawSession) error {
	switch s.Kind.Site() {
	case claw.SiteRemote:
		return o.attachRemote(s)
	case claw.SiteLocal:
		o.attachLocal(s)
		return nil
	}
	return errs.Newf(errs.ClassModelFailure, "daemon.ClawUp",
		"claw %s reports no valid site", s.Kind.Type())
}

// attachRemote prepares a claw the operator opens rather than configures.
func (o *Orchestrator) attachRemote(s *ClawSession) error {
	proxy := s.Live.proxy
	if proxy == nil {
		return errs.Newf(errs.ClassWiring, "daemon.ClawUp", "no local listener")
	}

	b, browser := s.Kind.(claw.Browser)
	if browser {
		// A browser cannot present a bearer token, so the listener gains a
		// cookie-shaped credential for it. The local key stays mandatory
		// either way (FR-SEC-09): a page in the operator's browser can fire
		// requests at a loopback port, and here a request that fires is a
		// request that spends.
		token, err := secret.Generate(32)
		if err != nil {
			return err
		}
		proxy.EnableBrowserSession(token)
		s.URL, err = proxy.NewSessionURL()
		if err != nil {
			return err
		}

		// Only work resets the idle clock. Without this an open tab would
		// hold a GPU overnight on the strength of a reconnecting socket.
		proxy.CountsAsWork = b.CountsAsWork()
		o.emit("claw", "browser session ready")
	}

	if h, ok := s.Kind.(claw.Holder); ok {
		// Work that produces no requests still has to hold the clock: a job
		// submitted in one short call and computed for minutes is
		// indistinguishable from an abandoned rig to a timer counting
		// requests.
		ctx, cancel := context.WithCancel(context.Background())
		s.hold = cancel
		go h.HoldWhileBusy(ctx, holderEndpoint(s.Live, proxy.LocalPort()), s.Live.Activity())
	}
	return nil
}

// attachLocal wires the applications a local claw names.
//
// Never fatal to the rig. A rig that serves but could not edit an editor's
// config is still a rig (§16, ClassWiring), and refusing to bring one up over a
// failed write would be a worse outcome than the failed write.
func (o *Orchestrator) attachLocal(s *ClawSession) {
	l, ok := s.Kind.(claw.Local)
	if !ok {
		o.warn("wiring", "claw %s is local and names no clients to wire", s.Kind.Type())
		return
	}
	writers := l.Clients()
	if len(writers) == 0 {
		return
	}
	// One credential per client (FR-SEC-23), so a leaked config burns one of
	// them, one can be revoked without rewiring the rest, and a request
	// carries an identity — which is what lets the probe below say that *this*
	// application arrived rather than that something did.
	//
	// Derived rather than generated, so the same client gets the same
	// credential from the same rig token instead of a fresh one per call.
	var (
		recs    []core.WiringRecord
		errList []error
		probe   = wire.ProxyProber(s.Live.proxy)
	)
	for _, w := range writers {
		key, err := clientToken(s.Live.ClientToken, w.Name())
		if err != nil {
			// One cause shared by every writer — there is no base to derive
			// from — so it is reported once rather than once per client, and
			// nothing is wired rather than everything wired identically.
			errList = append(errList, err)
			break
		}
		if s.Live.proxy != nil {
			s.Live.proxy.AddClient(w.Name(), key)
		}
		// Applied one at a time because each client is handed its own key, and
		// an endpoint carries one.
		ep := wire.Endpoint{
			URL:   s.Live.Endpoint,
			Model: s.Plan.Model.ServedName,
			Key:   key,
		}
		r, e := wire.Apply([]wire.ClientWriter{w}, ep, probe)
		recs = append(recs, r...)
		errList = append(errList, e...)

		// What to paste, for a client LARRI does not configure itself. Said
		// here rather than left in the writer, because the operator is
		// watching this stream and will not read a struct.
		for _, line := range wire.InstructionsFor(w, ep) {
			o.emit("wiring", "%s", line)
		}
	}
	s.Wiring = recs
	s.clients = wire.Index(writers)
	if s.Live.Rig != nil {
		// Persisted on the rig, because the thing that has to be undone must
		// survive this process dying (§10.2 step 4).
		s.Live.Rig.Wiring = recs
		_ = o.Store.Save(s.Live.Rig)
	}
	for _, rec := range recs {
		state := "wired"
		if !rec.Verified {
			state = "wired (unverified)"
		}
		o.emit("wiring", "%s %s", rec.Client, state)
	}
	for _, err := range errList {
		o.warn("wiring", "%s", shortErr(err))
	}
}

// ClawDown ends a session and tears the rig down.
//
// The order is the requirement and it differs by site. A remote claw's results
// exist only on the host, so they are collected first; a local claw changed
// configuration on this machine, so that is put back first — before the
// instance goes, so there is no window in which a client points at a dead
// endpoint (invariant 3).
//
// Neither step may prevent the destroy. State is money (§4): files left behind
// are lost once, a config that could not be restored is recoverable from its
// backup, and a rig left alive bills until somebody notices.
func (o *Orchestrator) ClawDown(ctx context.Context, s *ClawSession,
	term *core.Termination) (*claw.Result, error) {

	if s == nil || s.Live == nil || s.Live.Rig == nil {
		return nil, errs.Newf(errs.ClassUnknown, "daemon.ClawDown", "no rig to destroy")
	}

	var res *claw.Result
	switch s.Kind.Site() {
	case claw.SiteRemote:
		res = o.collectClaw(ctx, s)
		if res != nil && !res.Complete() && term != nil {
			// Recorded on the termination rather than only printed, so the
			// answer to "where are my outputs" survives the session that
			// lost them.
			if term.Evidence == nil {
				term.Evidence = map[string]string{}
			}
			term.Evidence["outputs"] = res.Summary()
		}
	case claw.SiteLocal:
		o.revertClaw(s)
	}

	if s.hold != nil {
		s.hold()
	}
	err := o.Down(ctx, s.Live.Rig, term)
	_ = s.Close()
	return res, err
}

// collectClaw retrieves what a remote claw produced, and never returns an
// error: every caller is on its way to a destroy, and there is no failure here
// worth not destroying over.
func (o *Orchestrator) collectClaw(ctx context.Context, s *ClawSession) *claw.Result {
	r, ok := s.Kind.(claw.Remote)
	if !ok {
		return nil
	}
	sess := s.Live.Session()
	if sess == nil {
		o.warn("outputs", "no ssh session: whatever the host produced cannot be collected")
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, o.outputBudget())
	defer cancel()

	o.emit("outputs", "collecting into %s", s.OutputDir)
	res, err := r.Collect(cctx, sess, s.OutputDir, s.StartedAt.Add(-time.Minute))
	if err != nil {
		o.warn("outputs", "could not collect: %s — destroying anyway rather than leaving it billing",
			shortErr(err))
		return nil
	}
	if res.Complete() {
		o.emit("outputs", "%s", res.Summary())
		return res
	}
	o.warn("outputs", "%s — these are lost when the host goes", res.Summary())
	for name, why := range res.Failed {
		o.warn("outputs", "  %s: %s", name, why)
	}
	return res
}

// revertClaw puts back whatever a local claw changed on this machine.
func (o *Orchestrator) revertClaw(s *ClawSession) {
	if len(s.Wiring) == 0 {
		return
	}
	for _, err := range wire.Revert(s.Wiring, s.clients) {
		o.warn("wiring", "%s", shortErr(err))
	}
	o.emit("wiring", "reverted %d client(s)", len(s.Wiring))
	if s.Live.Rig != nil {
		s.Live.Rig.Wiring = nil
		_ = o.Store.Save(s.Live.Rig)
	}
}

// outputBudget bounds how long a teardown spends collecting before it destroys
// anyway. Zero means ten minutes.
//
// A ceiling rather than a best effort, because the two costs are not
// symmetric. Files left behind are lost once; a rig left alive because it was
// still copying them bills until somebody notices.
func (o *Orchestrator) outputBudget() time.Duration {
	if o.OutputBudget > 0 {
		return o.OutputBudget
	}
	return 10 * time.Minute
}

// Close releases the session's own machinery. It does not destroy the
// instance — that is ClawDown's job, and conflating them would make a closed
// browser look like a teardown.
func (s *ClawSession) Close() error {
	if s.hold != nil {
		s.hold()
	}
	if s.Live != nil {
		return s.Live.Close()
	}
	return nil
}

// NewSessionURL mints a fresh link for a browser-opened claw.
//
// Needed because the link is genuinely one-time now: opening the rig in a
// second browser needs a second link, and issuing one invalidates any earlier
// link nobody used. Empty for a claw that is configured rather than opened.
func (s *ClawSession) NewSessionURL() string {
	if s == nil || s.Live == nil || s.Live.proxy == nil {
		return ""
	}
	u, err := s.Live.proxy.NewSessionURL()
	if err != nil {
		return ""
	}
	return u
}

// Endpoint is the local address the session is published on.
func (s *ClawSession) Endpoint() string {
	if s == nil || s.Live == nil {
		return ""
	}
	return s.Live.Endpoint
}

// clientToken derives one client's credential from the rig's.
//
// Derived rather than generated so it is reproducible: the same client on the
// same rig always presents the same key, which is what lets a value the
// operator pasted once keep working for as long as that rig does.
//
// Stability follows the base, which config.ResolveClientKey persists, so a
// value the operator pasted once keeps working across teardowns and instance
// replacements. That is the property invariant 8 asks for and the reason this
// derives rather than generates: the same client on the same machine always
// presents the same key, and a different client never presents the same one.
//
// The rig token is the opposite and stays that way — ephemeral, one per rig,
// never seen by a client. The proxy is the boundary (FR-SEC-22).
//
// The derivation refuses an empty base, which is not a defensive nicety: HMAC
// with no key is a constant, so every installation would hand the same client
// name the same credential — one that authenticates nothing while looking like
// a key. Unreachable while every claw path sets a base, which is exactly when
// a guard earns its place.
func clientToken(base secret.Secret, name string) (secret.Secret, error) {
	if base.Empty() {
		return secret.Secret{}, errs.Newf(errs.ClassWiring, "daemon.wire",
			"no rig credential to derive %s's key from", name)
	}
	mac := hmac.New(sha256.New, []byte(base.Reveal()))
	mac.Write([]byte(name))
	return secret.New("lc-" + hex.EncodeToString(mac.Sum(nil))[:40]), nil
}

// holderEndpoint is the local address and credential a claw polls its own
// application with.
//
// LARRI's own credential, not the operator's. This traffic is LARRI's — it
// carries the probe header, so it cannot reset the clock it exists to hold
// open — and probeToken is the only one registered unconditionally.
// ClientToken is not: a rig whose stored client keys are readable leaves it
// empty, and every poll would then be unauthenticated, the hold would never
// engage, and a long render would be destroyed by the idle timer it is there
// to hold off.
func holderEndpoint(live *Live, port int) claw.LocalEndpoint {
	return claw.LocalEndpoint{
		Addr:  fmt.Sprintf("127.0.0.1:%d", port),
		Token: live.probeToken.Reveal(),
	}
}
