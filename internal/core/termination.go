// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

import "time"

// Actor is who decided that a rig should end.
//
// This axis carries most of the meaning, because it answers the question an
// operator actually asks when a rig is gone — did I do this, did my own
// settings do this, did they do this, or did the tool break — and that is what
// determines whether they change a flag, change provider, or file a bug.
type Actor string

const (
	ActorOperator Actor = "operator" // you asked for this
	ActorPolicy   Actor = "policy"   // a rule you configured fired
	ActorProvider Actor = "provider" // it was done to you
	ActorFault    Actor = "fault"    // LARRI could not continue
)

// ReasonCode is why a rig ended. Typed rather than free text, for the same
// reason the error taxonomy is typed: the value drives display in five
// surfaces, and a string would drift per call site and be unqueryable after
// the fact.
type ReasonCode string

const (
	ReasonOperatorRequest   ReasonCode = "operator-request"
	ReasonPanicSweep        ReasonCode = "panic-sweep"
	ReasonIdleTimeout       ReasonCode = "idle-timeout"
	ReasonBudgetCeiling     ReasonCode = "budget-ceiling"
	ReasonPreempted         ReasonCode = "preempted"
	ReasonHostFailure       ReasonCode = "host-failure"
	ReasonProvisionDeadline ReasonCode = "provision-deadline"
	ReasonBootstrapFailed   ReasonCode = "bootstrap-failed"
	ReasonOrphanSweep       ReasonCode = "orphan-sweep"
	ReasonInstanceGone      ReasonCode = "instance-gone" // the provider no longer holds it
)

// CostSummary is what a rig spent, and on what.
type CostSummary struct {
	TotalUSD     float64       `json:"total_usd"`
	ComputeUSD   float64       `json:"compute_usd"`
	StorageUSD   float64       `json:"storage_usd,omitempty"` // accrued while STOPPED
	BootUSD      float64       `json:"boot_usd,omitempty"`    // spent before READY
	Ran          time.Duration `json:"ran"`
	PriceHr      float64       `json:"price_hr"`
	ReachedReady bool          `json:"reached_ready"`
}

// Termination is the answer to "why is my rig gone".
//
// It is resolved at the moment of the decision and journalled with the
// teardown intent — never reconstructed from log lines afterwards, because a
// supervisor that destroys first and infers the motive later gets it wrong
// exactly when it matters, which is when several conditions were true at once.
type Termination struct {
	Actor    Actor             `json:"actor"`
	Code     ReasonCode        `json:"code"`
	At       time.Time         `json:"at"`
	Summary  string            `json:"summary"`            // one evidence-bearing line
	Evidence map[string]string `json:"evidence,omitempty"` // the facts behind Summary
	Cost     CostSummary       `json:"cost"`

	// Outputs says what became of a remote claw's results, and destroying one
	// without an answer is refused.
	//
	// It lives on the termination rather than in the caller because every
	// surface tears rigs down and only one of them had the check. `larri
	// down` asked; the MCP tool and the TUI called Down directly and
	// destroyed the only copy of somebody's renders without anyone deciding
	// to. A rule enforced in one front-end is in the wrong layer
	// (invariant 6), and this is the record that has to exist anyway.
	Outputs OutputDisposition `json:"outputs,omitempty"`
}

// OutputDisposition is what happened to results that existed only on the host.
type OutputDisposition string

const (
	// OutputsUndecided is the zero value, and the one Down refuses on for a
	// rig that holds results. Nobody has said what should happen to them.
	OutputsUndecided OutputDisposition = ""

	// OutputsCollected: retrieval ran. Whether it retrieved everything is a
	// separate question the Result answers — what matters here is that the
	// attempt was made before the host went.
	OutputsCollected OutputDisposition = "collected"

	// OutputsDiscarded: somebody decided to lose them, or there were none to
	// lose because the rig never got far enough to make any.
	OutputsDiscarded OutputDisposition = "discarded"
)

// EvidenceNothingCreated is the evidence key a teardown sets when it has
// asked the provider and found nothing carrying the rig's label.
//
// Cost reads it: the states a failed create passes through bill by default,
// deliberately, and this is what ends that assumption with a fact.
const EvidenceNothingCreated = "nothing_created"

// Automatic reports whether LARRI ended the rig without being asked.
// An automatic termination is exactly the case where the evidence matters,
// because nobody was watching when it happened.
func (t *Termination) Automatic() bool {
	return t != nil && (t.Actor == ActorPolicy || t.Actor == ActorFault)
}
