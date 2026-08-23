// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"time"

	"go.sovrenix.com/larri/internal/config"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/state"
)

// SupervisePolicy is what the supervisor enforces while a rig serves.
type SupervisePolicy struct {
	Idle   config.Idle
	Budget config.Budget

	// HealthInterval is how often a real completion is attempted (FR-SUP-01).
	// Zero means thirty seconds.
	HealthInterval time.Duration

	// PollInterval is how often the deadlines are evaluated. Zero means ten
	// seconds. Separate from HealthInterval because a health check costs a
	// generation and a deadline check costs arithmetic.
	PollInterval time.Duration

	// Warn is how much lead time a deadline warning gets (FR-SUP-09). Zero
	// means two minutes.
	Warn time.Duration
}

func (p SupervisePolicy) healthInterval() time.Duration {
	if p.HealthInterval > 0 {
		return p.HealthInterval
	}
	return 30 * time.Second
}

func (p SupervisePolicy) pollInterval() time.Duration {
	if p.PollInterval > 0 {
		return p.PollInterval
	}
	return 10 * time.Second
}

func (p SupervisePolicy) warnLead() time.Duration {
	if p.Warn > 0 {
		return p.Warn
	}
	return 2 * time.Minute
}

// Supervise watches a serving rig and enforces the deadlines that stop it
// costing money nobody meant to spend.
//
// It returns when the rig should stop being supervised: because the context
// ended (the operator is leaving), or because a policy decided the rig should
// die. In the latter case it returns the Termination that says who decided and
// why, and the caller performs the teardown — the supervisor does not destroy
// anything itself.
//
// That split is deliberate. A supervisor that both decides and destroys has two
// reasons to be wrong and one place to look; keeping the decision separate from
// the act means the reason is recorded before the spend stops, which is the
// order FR-DEL-08 requires and the order a crash between them can survive.
//
// A nil return means "stop watching, the rig is fine" — the operator
// interrupted, and an interrupt is not a reason to destroy anything.
func (o *Orchestrator) Supervise(ctx context.Context, live *Live, p SupervisePolicy) *core.Termination {
	if live == nil || live.proxy == nil {
		return nil
	}
	var (
		lastHealth  time.Time
		warnedIdle  bool
		warnedSpend bool
		failures    int
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.pollInterval()):
		}

		now := time.Now()

		// ---- budget --------------------------------------------------
		//
		// Checked before idle, because a rig can breach a ceiling while
		// perfectly busy, and the money is gone either way. The cost comes
		// from the journal rather than a running total, so it counts storage
		// accrued by a STOPPED rig too — the leak a GPU-hours-only ceiling
		// would never notice (§12.5).
		if p.Budget.MaxUSD > 0 {
			cost := o.accrued(live.Rig, now)
			if cost.TotalUSD >= p.Budget.MaxUSD {
				return &core.Termination{
					Actor: core.ActorPolicy, Code: core.ReasonBudgetCeiling,
					At:      now.UTC(),
					Summary: fmt.Sprintf("accrued $%.4f reached the $%.2f ceiling", cost.TotalUSD, p.Budget.MaxUSD),
					Evidence: map[string]string{
						"ceiling_usd": fmt.Sprintf("%.2f", p.Budget.MaxUSD),
						"total_usd":   fmt.Sprintf("%.4f", cost.TotalUSD),
						"compute_usd": fmt.Sprintf("%.4f", cost.ComputeUSD),
						"storage_usd": fmt.Sprintf("%.4f", cost.StorageUSD),
						"ran":         cost.Ran.Round(time.Second).String(),
					},
				}
			}
			// Warn once, with lead time measured in money rather than in
			// clock: the operator can act on "you have $0.40 left".
			if !warnedSpend && p.Budget.MaxUSD-cost.TotalUSD <= o.spendLead(live.Rig, p) {
				warnedSpend = true
				o.warn("budget", "$%.4f of $%.2f spent — %s until the ceiling destroys this rig",
					cost.TotalUSD, p.Budget.MaxUSD, o.timeToCeiling(live.Rig, p, cost.TotalUSD))
			}
		}

		// ---- idle ----------------------------------------------------
		//
		// FR-SUP-08: only operator-attributable inference counts. The health
		// check below runs every 30 s by design, and if it reset this clock
		// the timeout would never fire.
		if p.Idle.Timeout > 0 {
			idle := live.proxy.Activity.IdleFor(now)
			switch {
			case idle >= p.Idle.Timeout && p.Idle.Action == config.IdleDestroy:
				return &core.Termination{
					Actor: core.ActorPolicy, Code: core.ReasonIdleTimeout,
					At:      now.UTC(),
					Summary: fmt.Sprintf("no operator inference for %s (window %s)", idle.Round(time.Second), p.Idle.Timeout),
					Evidence: map[string]string{
						"idle_for":       idle.Round(time.Second).String(),
						"window":         p.Idle.Timeout.String(),
						"last_request":   live.proxy.Activity.LastOperatorRequest().Format(time.RFC3339),
						"requests_total": fmt.Sprint(live.proxy.Activity.Requests()),
						"probes_total":   fmt.Sprint(live.proxy.Activity.Probes()),
					},
				}
			case idle >= p.Idle.Timeout:
				// warn mode: say it once per idle stretch, not every poll.
				if !warnedIdle {
					warnedIdle = true
					o.warn("idle", "no operator inference for %s; still billing at $%.3f/hr",
						idle.Round(time.Second), live.Rig.Offer.PriceHr)
				}
			case idle+p.warnLead() >= p.Idle.Timeout && p.Idle.Action == config.IdleDestroy:
				if !warnedIdle {
					warnedIdle = true
					o.warn("idle", "idle %s of %s — this rig is destroyed in %s unless used",
						idle.Round(time.Second), p.Idle.Timeout,
						(p.Idle.Timeout - idle).Round(time.Second))
				}
			default:
				warnedIdle = false // used again; re-arm the warning
			}
		}

		// ---- health --------------------------------------------------
		//
		// A real completion, not a liveness ping (FR-SUP-01): a runtime that
		// accepts TCP and answers /health while generating nothing is exactly
		// the failure a cheap check misses.
		if now.Sub(lastHealth) >= p.healthInterval() {
			lastHealth = now
			if err := o.probe(ctx, live); err != nil {
				failures++
				o.warn("health", "probe failed (%d in a row): %s", failures, shortErr(err))
				// Three consecutive failures is a rig that has stopped
				// serving. It is not automatically a rig to destroy — that is
				// FR-SUP-02's taxonomy and the operator's call — so the
				// supervisor reports and keeps watching rather than deciding.
				if failures == 3 {
					_ = o.Store.Transition(live.Rig, core.StateDegraded, "health probes failing")
				}
			} else {
				if failures > 0 {
					o.emit("health", "recovered after %d failed probes", failures)
					if live.Rig.State == core.StateDegraded {
						_ = o.Store.Transition(live.Rig, core.StateReady, "health probe recovered")
					}
				}
				failures = 0
			}
		}
	}
}

// probe runs one health completion through the local endpoint.
//
// Through the proxy rather than against the host, so a pass proves the whole
// path the operator uses. It carries ProbeHeader so it does not reset the idle
// clock it would otherwise make immortal (FR-SUP-08).
func (o *Orchestrator) probe(ctx context.Context, live *Live) error {
	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ep := runtime.Endpoint{
		Host: "127.0.0.1", Port: live.proxy.LocalPort(),
		Model: live.Rig.Model.ServedName, Key: live.ClientToken,
		Probe: true,
	}
	return o.Runtime.Ready(pctx, ep, live.Rig.Model)
}

// accrued reports what the rig has cost so far, replayed from the journal.
func (o *Orchestrator) accrued(rig *core.Rig, now time.Time) core.CostSummary {
	entries, err := o.Store.Entries()
	if err != nil {
		return core.CostSummary{}
	}
	return state.CostFor(entries, rig.ID, now)
}

// spendLead converts the warning lead time into money, so a ceiling warning
// arrives with time to act rather than after the fact.
func (o *Orchestrator) spendLead(rig *core.Rig, p SupervisePolicy) float64 {
	return rig.Offer.PriceHr * p.warnLead().Hours()
}

// timeToCeiling estimates how long the remaining budget lasts at the current
// rate. An estimate is honest here: the operator needs to know whether to act
// now or after lunch, not the exact second.
func (o *Orchestrator) timeToCeiling(rig *core.Rig, p SupervisePolicy, spent float64) time.Duration {
	if rig.Offer.PriceHr <= 0 {
		return 0
	}
	remaining := p.Budget.MaxUSD - spent
	if remaining <= 0 {
		return 0
	}
	return time.Duration(remaining / rig.Offer.PriceHr * float64(time.Hour)).Round(time.Second)
}
