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
}

// Automatic reports whether LARRI ended the rig without being asked.
// An automatic termination is exactly the case where the evidence matters,
// because nobody was watching when it happened.
func (t *Termination) Automatic() bool {
	return t != nil && (t.Actor == ActorPolicy || t.Actor == ActorFault)
}
