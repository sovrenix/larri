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
		{At: at(0), RigID: "r", To: core.StateCreating, PriceHr: 1.20, StorageHr: 0.01, Rates: RatesBilled},
		{At: at(6), RigID: "r", To: core.StateReady, PriceHr: 1.20, StorageHr: 0.01, Rates: RatesBilled},
		{At: at(66), RigID: "r", To: core.StateDraining, PriceHr: 1.20, StorageHr: 0.01, Rates: RatesBilled},
		{At: at(67), RigID: "r", To: core.StateDestroyed, PriceHr: 1.20, StorageHr: 0.01, Rates: RatesBilled},
	}
	c := Cost(entries, at(120))

	// 67 minutes billable at $1.20/hr all-in = $1.34, of which a cent is
	// storage.
	near(t, "total", c.TotalUSD, 1.34)
	near(t, "compute", c.ComputeUSD, 1.34-0.011)
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
		{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 1.00, StorageHr: 0.02, Rates: RatesBilled},
		{At: at(60), RigID: "r", To: core.StateStopped, PriceHr: 1.00, StorageHr: 0.02, Rates: RatesBilled},
	}
	c := Cost(entries, at(660)) // ten hours stopped

	near(t, "compute", c.ComputeUSD, 0.98) // only the first hour, less its storage
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

// FAILED bills by default, because a create that fails may still have created
// something. That is an assumption awaiting evidence — and a teardown that
// asked the provider and found nothing is the evidence. Without this, a create
// refused for a missing API key accrued $1.39/hr against a machine that never
// existed, and `larri status` was still reporting the total four days later.
func TestARigConfirmedNeverCreatedNeverBilled(t *testing.T) {
	start := time.Date(2026, 9, 8, 20, 5, 0, 0, time.UTC)
	entries := []Entry{
		{At: start, RigID: "r", To: core.StateCreating, PriceHr: 1.39},
		{At: start.Add(time.Second), RigID: "r", To: core.StateFailed, PriceHr: 1.39},
		{At: start.Add(80 * time.Hour), RigID: "r", To: core.StateDestroyed, PriceHr: 1.39,
			Termination: &core.Termination{
				Actor: core.ActorFault, Code: core.ReasonHostFailure,
				Evidence: map[string]string{
					core.EvidenceNothingCreated: "no instance carries this rig's label",
				},
			}},
	}
	c := Cost(entries, start.Add(100*time.Hour))
	if c.TotalUSD != 0 {
		t.Errorf("accrued $%.4f against a machine confirmed never to have existed", c.TotalUSD)
	}
}

// The same journal without that evidence keeps billing: absence has to be
// stated, never inferred from a quiet record.
func TestAnUnconfirmedFailureKeepsAccruing(t *testing.T) {
	start := time.Date(2026, 9, 8, 20, 5, 0, 0, time.UTC)
	entries := []Entry{
		{At: start, RigID: "r", To: core.StateCreating, PriceHr: 1.39},
		{At: start.Add(time.Second), RigID: "r", To: core.StateFailed, PriceHr: 1.39},
	}
	if c := Cost(entries, start.Add(2*time.Hour)); c.TotalUSD <= 0 {
		t.Error("a failed create with no evidence either way must be assumed to bill")
	}
}

// PriceHr is what a running machine bills, storage included — Vast's
// dph_total is. Counting storage again on top of it charged every running
// hour twice for the disk.
func TestARunningRigCostsItsBilledRateOnce(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 0.821, StorageHr: 0.0214, Rates: RatesBilled},
	}
	c := Cost(entries, at(60))
	near(t, "total", c.TotalUSD, 0.821)
	near(t, "storage", c.StorageUSD, 0.0214)
}

// Entries from before RatesBilled keep their meaning: the quote as the rate,
// storage on top, and storage as Vast's per-gigabyte-month price. Read as
// dollars an hour, an RTX 3060 quoted at $0.041/hr and billed $0.051/hr was
// costed at $0.174/hr — $0.1171 for forty minutes that cost about $0.034.
func TestEntriesFromBeforeBilledRatesAreReadByTheirOwnRules(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 0.041, StorageHr: 0.1333},
		{At: at(40), RigID: "r", To: core.StateDestroyed, PriceHr: 0.041, StorageHr: 0.1333},
	}
	c := Cost(entries, at(60))
	// $0.041/hr plus $0.1333/GB/month over 60 GB, for forty minutes.
	near(t, "total", c.TotalUSD, (0.041+0.1333*60/720)*40/60)
	if c.TotalUSD > 0.05 {
		t.Errorf("total $%.4f; the storage figure was read as dollars an hour", c.TotalUSD)
	}
}

// An old entry carries the quote, but the rig file kept the billed rate, and
// that is the one that was charged. Pod 01M26YFC read $2.69 at the $2.78
// quote; RunPod's billing history shows $3.10 for the same 58 minutes.
func TestAnOldEntryIsCostedAtTheRateTheRigRecordsAsBilled(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 2.78},
		{At: at(58), RigID: "r", To: core.StateDestroyed, PriceHr: 2.78},
	}
	rig := &core.Rig{ID: "r", Instance: &core.Instance{PriceHr: 3.18}}
	near(t, "total", CostForRig(entries, rig, at(60)).TotalUSD, 3.18*58/60)

	// A new entry already holds the billed rate and is left alone.
	entries[0].Rates, entries[1].Rates = RatesBilled, RatesBilled
	entries[0].PriceHr, entries[1].PriceHr = 3.00, 3.00
	near(t, "billed entries", CostForRig(entries, rig, at(60)).TotalUSD, 3.00*58/60)
}

// A stale writer's entry after the end changes nothing: the live run that
// found it journalled READY→DEGRADED a minute after DESTROYED, and replaying
// it billed $0.49/hr for an A40 that was gone.
func TestEntriesAfterARigEndedDoNotBill(t *testing.T) {
	entries := []Entry{
		{At: at(0), RigID: "r", To: core.StateReady, PriceHr: 0.49, Rates: RatesBilled},
		{At: at(15), RigID: "r", To: core.StateDestroyed, PriceHr: 0.49, Rates: RatesBilled},
		{At: at(16), RigID: "r", From: core.StateReady, To: core.StateDegraded, PriceHr: 0.49, Rates: RatesBilled},
	}
	near(t, "total", Cost(entries, at(120)).TotalUSD, 0.49*15/60)

	// Nor does a second record of the end, which the stale writer also made.
	entries = append(entries, Entry{At: at(26), RigID: "r", From: core.StateDegraded,
		To: core.StateDestroyed, Rates: RatesBilled, Termination: &core.Termination{}})
	entries[1].Termination = &core.Termination{}
	c := Cost(entries, at(120))
	if c.Ran != 15*time.Minute {
		t.Errorf("ran %s; the rig ended at the first DESTROYED, fifteen minutes in", c.Ran)
	}
}
