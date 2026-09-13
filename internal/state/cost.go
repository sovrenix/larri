// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"math"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

// legacyDiskGB and hoursPerMonth read the storage figure in entries written
// before RatesBilled: Vast's per-gigabyte-month price, over the smallest disk
// any rental of that period had. The disk itself was never journalled, and
// the floor understates a larger one by cents rather than overstating every
// rig by the $0.20/hr the raw figure read as.
const (
	legacyDiskGB  = 60
	hoursPerMonth = 720
)

// Cost reconstructs what a rig spent, from the journal alone.
//
// Derived rather than counted, and that is deliberate (FR-STATE-03). A running
// total in memory is lost when the daemon restarts, and a running total on
// disk is a second source of truth that can drift from the transitions it
// claims to summarise. Replaying the journal gives the same answer every time,
// survives any crash, and is auditable line by line.
//
// Compute and storage are tracked separately because they stop at different
// moments: compute ends when the instance stops running, storage only when the
// resource is destroyed. A rig that sat STOPPED for a day accrued no compute
// and real storage, and a summary that folded them together would report that
// day as free.
func Cost(entries []Entry, now time.Time) core.CostSummary {
	return cost(entries, now, 0)
}

// cost replays entries. billedHr, when known, is the rate the provider billed
// the rig, and replaces the quote in entries written before RatesBilled.
func cost(entries []Entry, now time.Time, billedHr float64) core.CostSummary {
	var (
		sum       core.CostSummary
		state     core.LifecycleState
		since     time.Time
		priceHr   float64
		storageHr float64
		firstAt   time.Time
		readyAt   time.Time
		haveState bool

		// confirmedAbsent records that a termination stated, as evidence,
		// that the provider held nothing carrying this rig's label.
		confirmedAbsent bool

		// legacy is whether the latest rates were written before RatesBilled.
		legacy bool
	)

	accrue := func(until time.Time) {
		if !haveState || since.IsZero() || until.Before(since) {
			return
		}
		hours := until.Sub(since).Hours()
		if computeBilling(state) {
			// PriceHr is everything billed while the machine runs, storage
			// included, so compute is what is left once storage is counted
			// on its own line. Adding the two double-counted storage. An
			// entry from before that rule held the quote, with storage on
			// top.
			if legacy && billedHr == 0 {
				sum.ComputeUSD += hours * priceHr
			} else {
				sum.ComputeUSD += hours * math.Max(priceHr-storageHr, 0)
			}
		}
		if storageBilling(state) {
			sum.StorageUSD += hours * storageHr
		}
	}

	for _, e := range entries {
		if firstAt.IsZero() {
			firstAt = e.At
		}
		if e.Termination != nil && e.Termination.Evidence[core.EvidenceNothingCreated] != "" {
			confirmedAbsent = true
		}
		accrue(e.At)

		// Nothing follows the end of a rig. A process holding a stale copy
		// once journalled DEGRADED after another had journalled DESTROYED,
		// and replaying that entry billed a pod that no longer existed.
		ended := haveState && state == core.StateDestroyed
		if ended && e.To != core.StateDestroyed {
			continue
		}
		legacy = e.Rates != RatesBilled
		if e.PriceHr > 0 {
			priceHr = e.PriceHr
			if legacy && billedHr > 0 {
				priceHr = billedHr
			}
		}
		if e.StorageHr > 0 {
			storageHr = e.StorageHr
			if legacy {
				storageHr = e.StorageHr * legacyDiskGB / hoursPerMonth
			}
		}
		state, haveState = e.To, true
		if !ended {
			since = e.At // a later record of the same end does not move it
		}

		if e.To == core.StateReady && readyAt.IsZero() {
			readyAt = e.At
			// Everything spent before the first READY was paid to get there:
			// image pull, weight download, launch. That is the figure R-03 is
			// about, so it is reported separately rather than buried.
			sum.BootUSD = sum.ComputeUSD + sum.StorageUSD
			sum.ReachedReady = true
		}
		if e.Termination != nil && !ended {
			sum.Ran = e.At.Sub(firstAt)
		}
	}

	// A rig still in a billing state is still accruing, so bring it up to now.
	if haveState && state.Billable() && state != core.StateDestroyed {
		accrue(now)
		sum.Ran = now.Sub(firstAt)
	} else if sum.Ran == 0 && !firstAt.IsZero() && !since.IsZero() {
		sum.Ran = since.Sub(firstAt)
	}

	// A rig whose teardown recorded that nothing was ever created never
	// billed, whatever states it passed through. FAILED counts as billing on
	// purpose — a create that fails may still have created something — but
	// that is an assumption awaiting evidence, and this is the evidence
	// arriving: LARRI asked the provider and nothing carried the rig's label.
	//
	// Keyed on what a termination recorded, never on a missing instance id.
	// Inferring absence from a blank field would zero real rigs whose journal
	// happens not to name a machine, and understating money is the one
	// direction this package must not fail in.
	//
	// Without it the assumption outlived its purpose: a create refused for a
	// missing API key left a rig accruing $1.39/hr against a machine that
	// never existed, and `larri status` still reported the total four days
	// later, by then $111.
	if confirmedAbsent {
		sum.ComputeUSD, sum.StorageUSD, sum.BootUSD = 0, 0, 0
	}

	sum.PriceHr = priceHr
	sum.TotalUSD = sum.ComputeUSD + sum.StorageUSD
	return sum
}

// computeBilling reports whether GPU time is being charged in this state.
func computeBilling(s core.LifecycleState) bool {
	switch s {
	case core.StateCreating, core.StateProvisioned, core.StateBootstrapping,
		core.StateReady, core.StateDegraded, core.StateDraining,
		core.StateFailed, core.StateOrphaned:
		return true
	default:
		// STOPPED explicitly excluded: the GPU was released, so compute stops.
		return false
	}
}

// storageBilling reports whether storage is being charged.
//
// STOPPED is included, and it is the whole reason this function exists
// separately. The container still exists, so storage still bills — on some
// providers at a higher rate than while running. Only absence from the
// provider's inventory ends it (R-13).
func storageBilling(s core.LifecycleState) bool {
	switch s {
	case core.StateIdle, core.StateSearching, core.StateSelected, core.StateDestroyed:
		return false
	default:
		return true
	}
}

// CostFor reconstructs one rig's cost from a full journal.
func CostFor(entries []Entry, rigID string, now time.Time) core.CostSummary {
	return Cost(EntriesFor(entries, rigID), now)
}

// CostForRig is CostFor with what the rig's snapshot knows and old journal
// entries do not: the rate the provider billed.
//
// Entries written before RatesBilled carry the quote, and the quote was not
// the bill — a RunPod pod quoted at $2.78/hr billed $3.18/hr, and its record
// read $2.69 against the $3.10 RunPod's own billing history shows. The
// snapshot kept the billed rate all along, storage included, so an old entry
// read with it follows the current rules.
func CostForRig(entries []Entry, rig *core.Rig, now time.Time) core.CostSummary {
	var billed float64
	if rig.Instance != nil {
		billed = rig.Instance.PriceHr
	}
	return cost(EntriesFor(entries, rig.ID), now, billed)
}
