// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

// LifecycleState is the canonical rig state machine of LARRI-REQ-001 §6.
type LifecycleState string

const (
	StateIdle          LifecycleState = "IDLE"
	StateSearching     LifecycleState = "SEARCHING"
	StateSelected      LifecycleState = "SELECTED"
	StateCreating      LifecycleState = "CREATING"
	StateProvisioned   LifecycleState = "PROVISIONED"
	StateBootstrapping LifecycleState = "BOOTSTRAPPING"
	StateReady         LifecycleState = "READY"
	StateDegraded      LifecycleState = "DEGRADED"

	// StateStopped: the instance exists but is not running — outbid, stopped
	// by the host, or halted for an exhausted balance.
	//
	// It is named for what is true rather than for why, because outbidding, a
	// host power-cycling a machine, and a spent balance all land here and all
	// bill identically. It is deliberately not called PREEMPTED: that name
	// sounded terminal, and this state is neither terminal nor free. The
	// container still exists, so storage still bills — on some providers at a
	// higher rate than while running.
	StateStopped LifecycleState = "STOPPED"

	StateDraining  LifecycleState = "DRAINING"
	StateDestroyed LifecycleState = "DESTROYED"
	StateFailed    LifecycleState = "FAILED"
	StateOrphaned  LifecycleState = "ORPHANED"
)

// Billable reports whether a state costs money.
//
// The rule from §6: any state whose billability is "assume yes" is treated as
// costing money until the provider is re-queried and proves otherwise. Only
// DESTROYED — the provider affirmatively reporting the resource absent — ends
// billing. Stopped is not gone.
func (s LifecycleState) Billable() bool {
	switch s {
	case StateIdle, StateSearching, StateSelected, StateDestroyed:
		return false
	default:
		// CREATING and FAILED included: assume yes until proven otherwise.
		return true
	}
}

// ExpectsInstance reports whether a live resource at the provider is the
// normal situation for this state, rather than a contradiction of it.
//
// Distinct from Billable, which answers a different question and was once
// mistaken for this one. Billable is a cost rule — "assume it costs money
// until proven otherwise" — so CREATING and FAILED are both billable. But a
// rig that FAILED and still has an instance running is precisely the leak the
// orphan sweep exists to catch, while a rig that is BOOTSTRAPPING and has one
// is simply working.
//
// Getting that backwards listed a live bring-up as an orphan and offered to
// destroy it.
func (s LifecycleState) ExpectsInstance() bool {
	switch s {
	case StateCreating, StateProvisioned, StateBootstrapping,
		StateReady, StateDegraded, StateDraining, StateStopped:
		return true
	}
	return false
}

// Terminal reports whether no further transition is expected.
//
// STOPPED is not terminal: it requires a destroy decision. FAILED is not
// terminal either, because a failed rig may still have a billable instance
// behind it that teardown must reach.
func (s LifecycleState) Terminal() bool { return s == StateDestroyed }

// Tristate expresses require / forbid / allow for a criterion.
type Tristate int

const (
	// Forbid is the zero value, so an unset Tristate is the conservative one.
	// Interruptible offers default to forbidden (Q-04) for exactly this
	// reason: a preempted instance is stopped rather than destroyed, still
	// billing storage, and able to resume into a second billing instance.
	Forbid Tristate = iota
	Allow
	Require
)

func (t Tristate) String() string {
	switch t {
	case Allow:
		return "allow"
	case Require:
		return "require"
	default:
		return "forbid"
	}
}

// Permits reports whether a candidate with the given property satisfies t.
func (t Tristate) Permits(has bool) bool {
	switch t {
	case Require:
		return has
	case Allow:
		return true
	default:
		return !has
	}
}
