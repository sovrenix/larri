// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/daemon"
)

type logsArgs struct {
	Rig  string `json:"rig"`
	Tail int    `json:"tail"`
}

func (d Deps) logs(ctx context.Context, raw json.RawMessage) (any, error) {
	var a logsArgs
	_ = json.Unmarshal(raw, &a)
	if a.Tail <= 0 {
		a.Tail = 100
	}
	live := d.live()
	if live == nil {
		return nil, fmt.Errorf("no rig is being served by this process; logs need its ssh session")
	}
	o, err := d.NewOrchestrator(string(live.Rig.Runtime))
	if err != nil {
		return nil, err
	}
	rc, err := o.RuntimeLogs(ctx, live, a.Tail)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return nil, err
	}
	return map[string]any{"rig": live.Rig.ID, "log": string(b)}, nil
}

func (d Deps) live() *daemon.Live {
	if d.Session == nil {
		return nil
	}
	return d.Session.Live()
}

func (d Deps) orphans(ctx context.Context, _ json.RawMessage) (any, error) {
	o, err := d.NewOrchestrator("")
	if err != nil {
		return nil, err
	}
	orphans, err := o.Orphans(ctx)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	var hourly float64
	for _, orph := range orphans {
		rows = append(rows, map[string]any{
			"instance_id": orph.Instance.InstanceID,
			"running":     orph.Instance.Running,
			"price_hr":    orph.Instance.PriceHr,
			"status":      orph.Instance.Status,
			"reason":      orph.Reason,
			"describe":    orph.Describe(),
		})
		hourly += orph.Instance.PriceHr
	}
	return map[string]any{
		"orphans": rows, "count": len(rows),
		"total_price_hr": round4(hourly),
		"note":           "nothing was destroyed; larri_orphan_destroy removes one",
	}, nil
}

type upArgs struct {
	offersArgs
	IdleTimeout string  `json:"idle_timeout"`
	Budget      float64 `json:"budget"`
	Runtime     string  `json:"runtime"`
	DryRun      bool    `json:"dry_run"`
}

// up rents hardware, serves a model on it, and holds the tunnel.
//
// It starts the work and returns; it does not block until the rig is ready. A
// bring-up can take twenty minutes — an image pull, a weight download, a
// checkpoint load — and a tool call that blocked for twenty minutes would be
// indistinguishable from a hung server. The agent polls larri_status instead,
// which can say *what* is happening rather than only whether it finished.
//
// Holding the tunnel is what makes this different from provisioning. An
// earlier version called Up rather than UpAndServe and left the instance at
// PROVISIONED: billing by the second, with no runtime, no model, and no way to
// reach it — `larri resume` refuses it, correctly, because there is no server
// to adopt. That is renting hardware that cannot serve.
func (d Deps) up(ctx context.Context, raw json.RawMessage) (any, error) {
	var a upArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	// The cost-safety guard runs first, and before anything about *this*
	// surface, because a second rig alongside a billing one is the leak this
	// product exists to prevent. An agent is likelier to cause it than a
	// person, because it cannot see the terminal it did not print to.
	if rigs, err := d.Store.List(); err == nil {
		for _, r := range rigs {
			if r.State.Billable() {
				return nil, fmt.Errorf("rig %s is already billing at $%.3f/hr; "+
					"destroy it with larri_down before renting another", r.ID, r.Offer.PriceHr)
			}
		}
	}
	if d.Session == nil {
		return nil, fmt.Errorf("this surface cannot hold a rig")
	}

	crit := core.Criteria{MaxPriceHr: a.MaxPrice, MinReliability: 0.90, DiskGB: 60}
	if a.GPU != "" {
		crit.GPUModel = []string{a.GPU}
	}
	spec := a.spec()
	spec.ServedName = "larri"

	if a.DryRun {
		o, err := d.NewOrchestrator(a.Runtime)
		if err != nil {
			return nil, err
		}
		sv, err := o.Offers(ctx, daemon.UpRequest{Criteria: crit, Model: spec, DiskGB: 60})
		if err != nil {
			return nil, err
		}
		res := map[string]any{"dry_run": true, "spent": 0.0, "offers_considered": sv.Offers}
		if sv.Selection.Selected != nil {
			res["would_rent"] = map[string]any{
				"gpu":      sv.Selection.Selected.Offer.GPUModel,
				"price_hr": round4(sv.Selection.Selected.Offer.PriceHr),
			}
		}
		return res, nil
	}

	// Its own context, because the work outlives the call that started it.
	// Tying it to the tool call's context would cancel the bring-up the
	// moment the reply was sent, leaving a half-provisioned instance.
	runCtx, cancel := context.WithCancel(context.Background())
	if !d.Session.Begin("", cancel) {
		cancel()
		snap := d.Session.Snapshot()
		return nil, fmt.Errorf("a rig is already being brought up (%s, %s); "+
			"poll larri_status or stop it with larri_down", snap.RigID, snap.Phase)
	}

	policy := d.policy(a)
	go d.bringUp(runCtx, crit, spec, a, policy)

	return map[string]any{
		"started": true,
		"billing": true,
		"model":   a.Model,
		"note": "provisioning has begun and the rig bills from the moment an instance " +
			"exists. Poll larri_status until state is READY, then use the endpoint it " +
			"returns. larri_down stops it at any point.",
		"poll": "larri_status",
	}, nil
}

// policy renders the deadlines an agent asked for.
func (d Deps) policy(a upArgs) daemon.SupervisePolicy {
	cfg := config.Default()
	if a.IdleTimeout != "" {
		if dur, err := time.ParseDuration(a.IdleTimeout); err == nil {
			cfg.Idle.Timeout = dur
		}
	}
	cfg.Budget.MaxUSD = a.Budget
	return daemon.SupervisePolicy{Idle: cfg.Idle, Budget: cfg.Budget}
}

// bringUp provisions, serves, and then supervises until something ends it.
//
// The supervisor runs here for the same reason it runs under `larri up`: an
// agent that walks away from a rig is exactly the case idle reclamation exists
// for, and a surface that skipped it would be the one where forgetting is
// cheapest.
func (d Deps) bringUp(ctx context.Context, crit core.Criteria, spec core.ModelSpec,
	a upArgs, policy daemon.SupervisePolicy) {

	o, err := d.NewOrchestrator(a.Runtime)
	if err != nil {
		d.Session.Fail(err)
		return
	}
	events := make(chan daemon.Event, 128)
	o.Events = events
	go func() {
		for e := range events {
			d.Session.Note(e.Phase, e.Message)
		}
	}()
	defer close(events)

	live, err := o.UpAndServe(ctx, daemon.UpRequest{
		Criteria: crit, Model: spec, DiskGB: 60,
		HFToken: d.HFToken, LocalPort: 0,
	})
	if err != nil {
		d.Session.Fail(err)
		return
	}
	d.Session.Ready(live)

	term := o.Supervise(ctx, live, policy)
	live.Close()

	// A policy that ended the rig destroys it; a cancelled context means the
	// operator asked to stop holding it, and larri_down does the destroying.
	if term != nil {
		dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer dcancel()
		_ = o.Down(dctx, live.Rig, term)
	}
	d.Session.Finish()
}

type downArgs struct {
	Rig string `json:"rig"`
}

func (d Deps) down(ctx context.Context, raw json.RawMessage) (any, error) {
	var a downArgs
	_ = json.Unmarshal(raw, &a)

	rigs, err := d.Store.List()
	if err != nil {
		return nil, err
	}
	var target *core.Rig
	for _, r := range rigs {
		if a.Rig != "" && r.ID != a.Rig {
			continue
		}
		if r.State.Billable() {
			target = r
			break
		}
	}
	if target == nil {
		// A bring-up can be in flight before any instance exists — between
		// the search and the create there is nothing billable to find, and a
		// `down` that reported "nothing to tear down" while provisioning
		// continued would be the one command that made the leak worse.
		if d.Session != nil && d.Session.Snapshot().Running {
			d.Session.Stop()
			return map[string]any{
				"destroyed": false, "stopped": true,
				"note": "no instance existed yet; the bring-up was cancelled. " +
					"Run larri_orphans to confirm nothing was created.",
			}, nil
		}
		return map[string]any{"destroyed": false, "note": "nothing billable to tear down"}, nil
	}
	// Stop holding it before destroying it. A supervisor still running
	// against a rig that is being torn down would race the teardown, and the
	// tunnel would be closed twice.
	if d.Session != nil {
		d.Session.Stop()
	}
	o, err := d.NewOrchestrator(string(target.Runtime))
	if err != nil {
		return nil, err
	}
	term := &core.Termination{
		Actor: core.ActorOperator, Code: core.ReasonOperatorRequest,
		At: time.Now().UTC(), Summary: "requested through the mcp tool surface",
	}
	if err := o.Down(ctx, target, term); err != nil {
		return nil, err
	}
	if d.Session != nil {
		d.Session.Finish()
	}
	c := target.End.Cost
	return map[string]any{
		"destroyed": true, "rig": target.ID,
		"ran":         c.Ran.Round(time.Second).String(),
		"total_usd":   round4(c.TotalUSD),
		"compute_usd": round4(c.ComputeUSD),
		"storage_usd": round4(c.StorageUSD),
		"note":        "absence confirmed at the provider; billing has stopped",
	}, nil
}

type orphanDestroyArgs struct {
	InstanceID string `json:"instance_id"`
}

func (d Deps) orphanDestroy(ctx context.Context, raw json.RawMessage) (any, error) {
	var a orphanDestroyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.InstanceID == "" {
		return nil, fmt.Errorf("instance_id is required")
	}
	o, err := d.NewOrchestrator("")
	if err != nil {
		return nil, err
	}
	if err := o.DestroyOrphan(ctx, a.InstanceID); err != nil {
		return nil, err
	}
	return map[string]any{
		"destroyed": true, "instance_id": a.InstanceID,
		"note": "absence confirmed at the provider",
	}, nil
}
