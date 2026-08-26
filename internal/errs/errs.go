// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package errs is the error taxonomy of LARRI-DES-001 §16.
//
// The classification drives retry policy, so it is a type rather than a
// convention. The class that costs real money if mishandled is
// ClassProviderUnknownOutcome: a mutation whose result is unknown must never be
// blind-retried, because the retry is how one instance becomes two.
package errs

import (
	"errors"
	"fmt"
)

// Class is the retry-relevant category of a failure.
type Class int

const (
	// ClassUnknown is the zero value and is treated as non-retryable.
	ClassUnknown Class = iota

	// ClassCriteriaUnsatisfiable: no offer can fit the model. Reject
	// pre-spend, naming the VRAM shortfall (NFR-11).
	ClassCriteriaUnsatisfiable

	// ClassProviderTransient: rate limit, 5xx, timeout on a read. Retry with
	// backoff; if it wraps a mutation, reconcile first.
	ClassProviderTransient

	// ClassProviderUnknownOutcome: a mutation whose result is unknown. Never
	// blind-retry. Reconcile by label, then decide (R-07).
	ClassProviderUnknownOutcome

	// ClassHostFailure: host-attributable — sshd never came up, GPU not
	// visible, image pull failed. Fall back to the next offer (FR-PROV-05).
	ClassHostFailure

	// ClassModelFailure: model- or config-attributable — bad ref, gated,
	// unsupported weight format, OOM at load. Do not retry on another host;
	// the next one fails identically.
	ClassModelFailure

	// ClassDestroyUnconfirmed: destroy issued, absence not proven. Retry to
	// deadline, then escalate loudly and keep the rig in the reconciler's
	// work list (FR-DEL-04).
	ClassDestroyUnconfirmed

	// ClassWiring: client config could not be written or reverted. Never
	// fatal to the rig; report, keep the backup, continue.
	ClassWiring

	// ClassSecurity: an identity check failed — a host key that no longer
	// matches the one a rig was pinned to, or a credential that did not come
	// from LARRI.
	//
	// Separate from ClassHostFailure because the responses differ in kind. A
	// host failure means try another machine; this means stop and tell the
	// operator, since falling back would quietly discard the only evidence
	// that something answered in place of the expected host.
	ClassSecurity
)

var classNames = map[Class]string{
	ClassUnknown:                "unknown",
	ClassCriteriaUnsatisfiable:  "criteria-unsatisfiable",
	ClassProviderTransient:      "provider-transient",
	ClassProviderUnknownOutcome: "provider-unknown-outcome",
	ClassHostFailure:            "host-failure",
	ClassModelFailure:           "model-failure",
	ClassDestroyUnconfirmed:     "destroy-unconfirmed",
	ClassWiring:                 "wiring",
	ClassSecurity:               "security",
}

func (c Class) String() string {
	if n, ok := classNames[c]; ok {
		return n
	}
	return fmt.Sprintf("class(%d)", int(c))
}

// Retryable reports whether retrying the same call unchanged is safe.
//
// ClassProviderUnknownOutcome is deliberately false: it is the one class where
// the safe-looking action is the expensive one. Reconcile, then decide.
func (c Class) Retryable() bool { return c == ClassProviderTransient }

// FatalToRig reports whether the rig cannot continue. Wiring failures are not
// fatal — a rig that serves but could not edit an IDE config is still a rig.
func (c Class) FatalToRig() bool {
	switch c {
	case ClassWiring, ClassProviderTransient:
		return false
	default:
		return true
	}
}

// Error carries a Class alongside the underlying cause.
type Error struct {
	Class Class
	Op    string // the operation that failed, e.g. "vastai.Create"
	Err   error
}

func (e *Error) Error() string {
	if e.Op != "" {
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Class, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a classified error.
func New(class Class, op string, err error) *Error {
	return &Error{Class: class, Op: op, Err: err}
}

// Newf builds a classified error from a format string.
func Newf(class Class, op, format string, args ...any) *Error {
	return &Error{Class: class, Op: op, Err: fmt.Errorf(format, args...)}
}

// ClassOf reports the class of err, walking the wrap chain. An unclassified
// error is ClassUnknown, which is not retryable — the safe default, since an
// error nobody classified is an error nobody reasoned about.
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassUnknown
}

// Is reports whether err carries the given class.
func Is(err error, class Class) bool { return ClassOf(err) == class }

// Retryable reports whether err may be retried unchanged.
func Retryable(err error) bool { return ClassOf(err).Retryable() }

// OpOf returns the operation that failed, or "" when the error does not name
// one. It lets a caller tell two failures apart by what broke rather than by
// the prose, which is what distinguishes a host that could not be reached
// from one whose hardware did not match its listing.
func OpOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Op
	}
	return ""
}
