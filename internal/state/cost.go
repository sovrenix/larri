// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"time"

	"go.sovrenix.com/larri/internal/core"
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
	var (
		sum       core.CostSummary
		state     core.LifecycleState
		since     time.Time
		priceHr   float64
		storageHr float64
		firstAt   time.Time
		readyAt   time.Time
		haveState bool
	)

	accrue := func(until time.Time) {
		if !haveState || since.IsZero() || until.Before(since) {
			return
		}
		hours := until.Sub(since).Hours()
		if computeBilling(state) {
			sum.ComputeUSD += hours * priceHr
		}
		if storageBilling(state) {
			sum.StorageUSD += hours * storageHr
		}
	}

	for _, e := range entries {
		if firstAt.IsZero() {
			firstAt = e.At
		}
		accrue(e.At)

		if e.PriceHr > 0 {
			priceHr = e.PriceHr
		}
		if e.StorageHr > 0 {
			storageHr = e.StorageHr
		}
		state, since, haveState = e.To, e.At, true

		if e.To == core.StateReady && readyAt.IsZero() {
			readyAt = e.At
			// Everything spent before the first READY was paid to get there:
			// image pull, weight download, launch. That is the figure R-03 is
			// about, so it is reported separately rather than buried.
			sum.BootUSD = sum.ComputeUSD + sum.StorageUSD
			sum.ReachedReady = true
		}
		if e.Termination != nil {
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
