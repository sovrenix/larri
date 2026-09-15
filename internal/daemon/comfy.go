// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/comfy"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/secret"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/workflow"
)

// ComfyRequest is an application session on rented hardware.
type ComfyRequest struct {
	// Name identifies the workflow in state and in the journal.
	Name string

	Graph  *workflow.Graph
	Bundle *workflow.Bundle

	// URLs is where each asset's bytes come from, resolved locally before
	// anything is rented.
	URLs map[string]string

	Criteria  core.Criteria
	LocalPort int
	HFToken   secret.Secret

	// OutputDir is the local directory rendered files are saved into.
	OutputDir string

	// SessionHours is how long the operator expects to work, and it is a
	// ranking input rather than a limit. The cheapest hourly rate is
	// routinely the dearest way to get a working endpoint, because the
	// bundle is downloaded at that rate too (§4b).
	SessionHours float64

	AllowPickle bool

	// ComfyUIRef pins the application version installed on the host. Empty
	// uses the adapter's default. Pinned rather than tracking a branch: the
	// image's torch and this ref have to work *together*, and a pair that
	// moves underneath you cannot be reproduced or debugged.
	ComfyUIRef string

	// Image overrides the container the adapter asks the provider for. Empty
	// uses its default. Named here rather than set on the adapter afterwards,
	// so the image the hardware floors were derived from is the image that is
	// actually rented.
	Image string

	Confirm func(offer core.Offer, plan core.SizingPlan) bool
}

// ComfySession is a ComfyUI rig held open for an operator to use.
type ComfySession struct {
	Live   *Live
	Client *comfy.Client

	// Graph is what this session exists to run, kept so a render can be
	// re-submitted without the caller reparsing the file.
	Graph *workflow.Graph

	// URL is the one-time link that logs a browser in. It carries the session
	// token, so it is printed once and not persisted.
	URL string

	// OutputDir is where renders are saved locally.
	OutputDir string

	// StartedAt bounds what a teardown collects, so a rig adopted from an
	// earlier session does not re-download files already held.
	StartedAt time.Time

	// Caveats are places where this rig's guarantees are weaker than usual.
	Caveats []string

	hold context.CancelFunc
}

// CriteriaFor derives hardware requirements from a graph's measured bundle.
//
// This is the step the whole feature turns on, and it is the one §4a demands:
// everything here is knowable from a JSON file and a repository listing, and
// every one of these floors, discovered after the rental instead of before it,
// is discovered at an hourly rate with a multi-gigabyte download already in
// flight.
func CriteriaFor(base core.Criteria, plan core.SizingPlan, b *workflow.Bundle) core.Criteria {
	c := base
	need := int((plan.RequiredVRAMBytes + sizing.GiB - 1) / sizing.GiB)
	if c.VRAMTotalGB < need {
		c.VRAMTotalGB = need
	}
	// Per-GPU rather than total, and that is the correction that matters
	// here: ComfyUI executes a graph on one device, so two 12 GB cards do not
	// hold a 20 GB bundle. Summing them would select hardware whose headroom
	// is arithmetic rather than real.
	if c.VRAMPerGPUGB < need {
		c.VRAMPerGPUGB = need
	}
	if b != nil {
		ram := int(sizing.DiffusionHostRAMBytes(b.TotalBytes) / sizing.GiB)
		if c.RAMGB < ram {
			c.RAMGB = ram
		}
		// Disk holds the image, the bundle, and the renders. Doubling the
		// bundle covers a partial file sitting beside its finished self
		// during a resumed fetch.
		disk := int(b.TotalBytes/sizing.GiB)*2 + 40
		if c.DiskGB < disk {
			c.DiskGB = disk
		}
	}
	return c
}

// ComfyUp rents hardware for a graph, stands ComfyUI up on it, and publishes
// it at a stable local port.
//
// Everything before the create call is a check that did not need one. The
// graph is parsed, its models are resolved to real repositories and measured,
// the VRAM requirement is computed from that, and the market is ranked on the
// cost of a *working endpoint* over the session rather than on the hourly rate
// — which for a bundle of this size is the difference between six minutes to
// ready and two hours (§4b).
func (o *Orchestrator) ComfyUp(ctx context.Context, req ComfyRequest) (*ComfySession, error) {
	if req.Graph == nil || req.Bundle == nil {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.ComfyUp",
			"no graph to run")
	}
	if req.OutputDir == "" {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.ComfyUp",
			"no local output directory")
	}

	rt := comfy.New(req.Graph, req.Bundle, req.URLs)
	rt.AllowPickle = req.AllowPickle
	if req.Image != "" {
		rt.ImageRef = req.Image
	}
	if req.ComfyUIRef != "" {
		rt.ComfyUIRef = req.ComfyUIRef
	}
	o.Runtime = rt

	plan, err := sizing.PlanDiffusion(sizing.DiffusionRequest{
		WeightBytes: req.Bundle.TotalBytes,
		Pixels:      req.Bundle.Image.Pixels(),
	})
	if err != nil {
		return nil, err
	}
	// The planner is what keeps survey from having to know what is running.
	// It re-runs per candidate so fit is judged against the card actually
	// under consideration rather than against nothing.
	o.Planner = func(ctx context.Context, _ UpRequest) (core.SizingPlan, error) {
		return sizing.PlanDiffusion(sizing.DiffusionRequest{
			WeightBytes: req.Bundle.TotalBytes,
			Pixels:      req.Bundle.Image.Pixels(),
		})
	}
	if req.SessionHours > 0 {
		o.Policy.SessionHours = req.SessionHours
	}

	o.emit("sizing", "%d models, %s to fetch; %s at %dx%d needs ~%s VRAM",
		len(req.Bundle.Items), sizing.HumanBytes(req.Bundle.TotalBytes),
		req.Name, req.Bundle.Image.Width, req.Bundle.Image.Height,
		sizing.HumanBytes(plan.RequiredVRAMBytes))
	for _, note := range rt.SecurityNotes() {
		o.warn("security", "%s", note)
	}

	up := UpRequest{
		Criteria:  CriteriaFor(req.Criteria, plan, req.Bundle),
		Model:     comfy.ModelSpecFor(req.Name, req.Graph),
		DiskGB:    req.Criteria.DiskGB,
		HFToken:   req.HFToken,
		LocalPort: req.LocalPort,
		Confirm:   req.Confirm,
	}
	started := time.Now()
	live, err := o.UpAndServe(ctx, up)
	if err != nil {
		return nil, err
	}

	sess, err := o.attachBrowser(live, rt, req, started)
	if err != nil {
		_ = live.Close()
		return nil, err
	}
	return sess, nil
}

// attachBrowser turns a served rig into something an operator can open.
func (o *Orchestrator) attachBrowser(live *Live, rt *comfy.Runtime,
	req ComfyRequest, started time.Time) (*ComfySession, error) {

	proxy := live.proxy
	if proxy == nil {
		return nil, errs.Newf(errs.ClassWiring, "daemon.ComfyUp", "no local listener")
	}

	// A browser cannot present a bearer token, so the listener gains a
	// cookie-shaped credential for it. The local key stays mandatory either
	// way (FR-SEC-09): a page in the operator's browser can fire requests at
	// a loopback port, and here a request that fires is a request that spends.
	browserToken, err := secret.Generate(32)
	if err != nil {
		return nil, err
	}
	proxy.EnableBrowserSession(browserToken)

	// Only work resets the idle clock. Without this an open tab would hold a
	// GPU overnight on the strength of a reconnecting WebSocket.
	proxy.CountsAsWork = comfy.CountsAsWork

	client := &comfy.Client{
		Addr:  comfy.LocalAddr("127.0.0.1", proxy.LocalPort()),
		Token: live.ClientToken.Reveal(),
		Probe: true,
	}
	s := &ComfySession{
		Live:      live,
		Client:    client,
		Graph:     req.Graph,
		URL:       proxy.SessionURL(),
		OutputDir: req.OutputDir,
		StartedAt: started,
		Caveats:   rt.Caveats(),
	}
	// A queued render holds no HTTP request open, so without this a graph
	// that takes longer than the idle timeout is destroyed halfway through.
	hctx, cancel := context.WithCancel(context.Background())
	s.hold = cancel
	go comfy.HoldWhileBusy(hctx, &comfy.Client{
		Addr: client.Addr, Token: client.Token, Probe: true,
	}, live.Activity(), 15*time.Second)

	for _, c := range s.Caveats {
		o.warn("ready", "%s", c)
	}
	o.emit("ready", "comfyui at %s", s.URL)
	return s, nil
}

// Close releases the session's own machinery. It does not destroy the
// instance — that is ComfyDown's job, and conflating them would make a closed
// browser look like a teardown.
func (s *ComfySession) Close() error {
	if s.hold != nil {
		s.hold()
	}
	if s.Live != nil {
		return s.Live.Close()
	}
	return nil
}

// ComfyDown collects what the session rendered and then tears the rig down.
//
// The order is the requirement: images first, destroy second, absence
// confirmed third. Renders exist only on the host, and a destroy is
// irreversible, so anything not collected before it is gone for good.
//
// What the retrieval must never do is prevent the teardown. State is money
// (§4): a rig that cannot be destroyed because it is still copying files is a
// rig that is still billing, and an operator who left for the evening would
// return to a bill rather than to their images. So the collection is bounded,
// it is attempted before anything is dismantled, and a failure is reported
// loudly and then proceeded past. The rig is destroyed either way, and the
// summary says exactly what was lost — which is the honest version of a
// trade-off that cannot be avoided, only chosen.
func (o *Orchestrator) ComfyDown(ctx context.Context, s *ComfySession,
	term *core.Termination) (*comfy.SyncResult, error) {

	var res *comfy.SyncResult
	if s != nil && s.Live != nil {
		res = o.collect(ctx, s)
	}
	if s != nil && s.hold != nil {
		s.hold()
	}
	var rig *core.Rig
	if s != nil && s.Live != nil {
		rig = s.Live.Rig
	}
	if rig == nil {
		return res, errs.Newf(errs.ClassUnknown, "daemon.ComfyDown", "no rig to destroy")
	}
	if res != nil && !res.Complete() {
		// Recorded on the termination rather than only printed, so the answer
		// to "where are my images" survives the session that lost them.
		if term != nil {
			if term.Evidence == nil {
				term.Evidence = map[string]string{}
			}
			term.Evidence["outputs"] = res.Summary()
		}
	}
	err := o.Down(ctx, rig, term)
	_ = s.Close()
	return res, err
}

// collect retrieves rendered files, and never returns an error.
//
// Deliberately: every caller is on its way to a destroy, and there is no
// failure here that is worth not destroying over. What there is instead is a
// report, and a loud one when it is incomplete.
func (o *Orchestrator) collect(ctx context.Context, s *ComfySession) *comfy.SyncResult {
	sess := s.Live.Session()
	if sess == nil {
		o.warn("outputs", "no ssh session: rendered images cannot be collected")
		return nil
	}
	o.emit("outputs", "collecting renders into %s", s.OutputDir)
	res, err := comfy.Sync(ctx, sess, s.OutputDir, comfy.SyncOptions{
		Since:  s.StartedAt.Add(-time.Minute),
		Budget: o.outputBudget(),
	})
	if err != nil {
		o.warn("outputs", "could not collect renders: %s — destroying anyway rather than leaving it billing",
			shortErr(err))
		return nil
	}
	if res.Complete() {
		o.emit("outputs", "%s (%s)", res.Summary(), sizing.HumanBytes(res.Bytes))
		return res
	}
	o.warn("outputs", "%s — these are lost when the host goes", res.Summary())
	for name, why := range res.Failed {
		o.warn("outputs", "  %s: %s", name, why)
	}
	return res
}

// OutputBudget bounds how long a teardown spends collecting. Zero means ten
// minutes.
func (o *Orchestrator) outputBudget() time.Duration {
	if o.OutputBudget > 0 {
		return o.OutputBudget
	}
	return 10 * time.Minute
}

// Render submits a graph and waits for it, holding the idle clock open for the
// duration.
//
// The hold is the point. A render is one short POST followed by minutes of
// work that produces no requests at all, and an idle timer measuring requests
// cannot tell that from an abandoned rig.
func (s *ComfySession) Render(ctx context.Context) (*comfy.HistoryEntry, error) {
	if s.Client == nil {
		return nil, errs.Newf(errs.ClassUnknown, "daemon.Render", "no client")
	}
	act := s.Live.Activity()
	act.EnterInFlight()
	defer act.ExitInFlight()

	if s.Graph == nil || !s.Graph.Format.Executable() {
		return nil, errs.Newf(errs.ClassModelFailure, "daemon.Render",
			"graph is not in the api serialisation: /prompt accepts no other")
	}
	res, err := s.Client.Submit(ctx, s.Graph.Raw, "larri-session")
	if err != nil {
		return nil, err
	}
	return s.Client.Await(ctx, res.PromptID, 2*time.Second)
}

// Endpoint is the local address the operator opens. A URL rather than an
// OpenAI base: this surface is browsed, not configured into a client.
func (s *ComfySession) Endpoint() string {
	if s.Live == nil || s.Live.Rig == nil {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/", s.Live.Rig.LocalPort)
}
