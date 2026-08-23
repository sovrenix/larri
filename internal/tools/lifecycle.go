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
	if d.Live == nil {
		return nil
	}
	return d.Live()
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

// up rents hardware. It is the tool that spends, so it says what it spent.
//
// It deliberately does not hold the tunnel: an MCP call returns, and a rig
// whose data plane died with the call would be a rig that bills and serves
// nothing. Bringing it up and reporting the rate leaves the operator's own
// `larri resume` as the way to attach — which is the same path a restart uses.
func (d Deps) up(ctx context.Context, raw json.RawMessage) (any, error) {
	var a upArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	// The same guard the CLI applies: a second rig alongside a billing one is
	// the leak this product exists to prevent, and an agent is more likely to
	// cause it than a person.
	if rigs, err := d.Store.List(); err == nil {
		for _, r := range rigs {
			if r.State.Billable() {
				return nil, fmt.Errorf("rig %s is already billing at $%.3f/hr; "+
					"destroy it with larri_down or reconnect with 'larri resume' before renting another",
					r.ID, r.Offer.PriceHr)
			}
		}
	}
	o, err := d.NewOrchestrator(a.Runtime)
	if err != nil {
		return nil, err
	}
	crit := core.Criteria{MaxPriceHr: a.MaxPrice, MinReliability: 0.90, DiskGB: 60}
	if a.GPU != "" {
		crit.GPUModel = []string{a.GPU}
	}
	spec := a.spec()
	spec.ServedName = "larri"

	if a.DryRun {
		sv, err := o.Offers(ctx, daemon.UpRequest{Criteria: crit, Model: spec, DiskGB: 60})
		if err != nil {
			return nil, err
		}
		res := map[string]any{"dry_run": true, "spent": 0.0, "offers_considered": sv.Offers}
		if sv.Selection.Selected != nil {
			res["would_rent"] = map[string]any{
				"gpu":      sv.Selection.Selected.Offer.GPUModel,
				"price_hr": sv.Selection.Selected.Offer.PriceHr,
			}
		}
		return res, nil
	}

	rig, err := o.Up(ctx, daemon.UpRequest{Criteria: crit, Model: spec, DiskGB: 60})
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"rig": rig.ID, "state": string(rig.State),
		"gpu": rig.Offer.GPUModel, "price_hr": rig.Offer.PriceHr,
		"provider": rig.Offer.Provider,
		"billing":  true,
		"note": fmt.Sprintf("this rig now bills at $%.3f/hr until destroyed; "+
			"attach to it with 'larri resume' or stop it with larri_down", rig.Offer.PriceHr),
	}
	if rig.Instance != nil {
		res["instance"] = rig.Instance.InstanceID
	}
	// A policy the agent asked for is worth confirming back, because the
	// agent will report it to a person who is relying on it.
	if a.IdleTimeout != "" {
		if dur, err := time.ParseDuration(a.IdleTimeout); err == nil {
			res["idle_timeout"] = dur.String()
			res["idle_note"] = "enforced while a larri process supervises this rig"
		}
	}
	if a.Budget > 0 {
		res["budget_usd"] = a.Budget
		res["budget_note"] = "enforced while a larri process supervises this rig"
	}
	_ = config.Default
	return res, nil
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
		return map[string]any{"destroyed": false, "note": "nothing billable to tear down"}, nil
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
