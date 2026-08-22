// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package state

import (
	"math"
	"testing"
	"time"

	"go.sovrenix.com/larri/internal/core"
)

func at(min int) time.Time {
	return time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func near(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.005 {
		t.Errorf("%s = $%.4f, want $%.4f", label, got, want)
	}
}

// Cost is replayed from the journal rather than counted, so it survives a
// restart and is auditable line by line (FR-STATE-03).
func TestCostIsDerivedFromTheJournal(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateCreating, PriceHr: 1.20, StorageHr: 0.01},
		{At: at(6), RigID: "r", To: core.StateReady, PriceHr: 1.20, StorageHr: 0.01},
		{At: at(66), RigID: "r", To: core.StateDraining, PriceHr: 1.20, StorageHr: 0.01},
		{At: at(67), RigID: "r", To: core.StateDestroyed, PriceHr: 1.20, StorageHr: 0.01},
	}
	c := Cost(entries, at(120))

	// 67 minutes billable at $1.20/hr = $1.34 compute.
	near(t, "compute", c.ComputeUSD, 1.34)
	// Boot is the 6 minutes before READY — the figure R-03 is about.
	near(t, "boot", c.BootUSD, 0.12+0.001)
	if !c.ReachedReady {
		t.Error("this rig reached READY")
	}
	if c.TotalUSD <= c.ComputeUSD {
		t.Error("total must include storage as well as compute")
	}
}

// A destroyed rig stops accruing. Replaying the same journal an hour later
// must give the same answer, or cost figures would drift after a restart.
func TestDestroyedRigStopsAccruing(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateCreating, PriceHr: 2.00},
		{At: at(30), RigID: "r", To: core.StateDestroyed, PriceHr: 2.00},
	}
	early := Cost(entries, at(31))
	late := Cost(entries, at(600))
	if math.Abs(early.TotalUSD-late.TotalUSD) > 1e-9 {
		t.Errorf("a destroyed rig must not accrue: $%.4f then $%.4f",
			early.TotalUSD, late.TotalUSD)
	}
	near(t, "compute", early.ComputeUSD, 1.00)
}

// The trap R-13 describes, in ledger form: a STOPPED rig releases the GPU but
// keeps its container, so compute stops and storage does not. Folding the two
// together would report a day spent STOPPED as free.
func TestStoppedAccruesStorageButNotCompute(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 1.00, StorageHr: 0.02},
		{At: at(60), RigID: "r", To: core.StateStopped, PriceHr: 1.00, StorageHr: 0.02},
	}
	c := Cost(entries, at(660)) // ten hours stopped

	near(t, "compute", c.ComputeUSD, 1.00) // only the first hour
	near(t, "storage", c.StorageUSD, 0.02*11)
	if c.TotalUSD <= c.ComputeUSD {
		t.Fatal("ten hours STOPPED must not be free")
	}
}

// A rig still running keeps accruing, which is what makes `larri status` show
// a live figure without a background counter.
func TestLiveRigAccruesToNow(t *testing.T) {
	entries := []Entry{{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 1.29}}
	near(t, "one hour", Cost(entries, at(60)).ComputeUSD, 1.29)
	near(t, "two hours", Cost(entries, at(120)).ComputeUSD, 2.58)
}

// A create whose response never landed still costs money, and the journal is
// the only record that it happened.
func TestCreateIntentAloneStillCosts(t *testing.T) {
	entries := []Entry{{At: at(0), RigID: "r", To: core.StateCreating, PriceHr: 1.50}}
	c := Cost(entries, at(20))
	if c.ComputeUSD <= 0 {
		t.Fatal("CREATING is assume-billable; an unanswered create is not free")
	}
	if c.ReachedReady {
		t.Error("this rig never reached READY")
	}
}

func TestCostForSelectsOneRig(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "a", To: core.StateReady, PriceHr: 1.00},
		{At: at(0), RigID: "b", To: core.StateReady, PriceHr: 4.00},
		{At: at(60), RigID: "a", To: core.StateDestroyed, PriceHr: 1.00},
		{At: at(60), RigID: "b", To: core.StateDestroyed, PriceHr: 4.00},
	}
	near(t, "rig a", CostFor(entries, "a", at(120)).ComputeUSD, 1.00)
	near(t, "rig b", CostFor(entries, "b", at(120)).ComputeUSD, 4.00)
}

func TestEmptyJournalCostsNothing(t *testing.T) {
	if c := Cost(nil, at(60)); c.TotalUSD != 0 {
		t.Errorf("no entries must cost nothing, got $%.4f", c.TotalUSD)
	}
}
