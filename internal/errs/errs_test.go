// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package errs

import (
	"errors"
	"fmt"
	"testing"
)

// The whole reason this package is typed rather than conventional: retry
// policy is derived from the class, so the derivation is what gets tested.
func TestRetryPolicy(t *testing.T) {
	cases := []struct {
		class  Class
		retry  bool
		fatal  bool
		whyNot string
	}{
		{ClassProviderTransient, true, false, ""},
		{ClassProviderUnknownOutcome, false, true,
			"blind retry is how one instance becomes two (R-07)"},
		{ClassCriteriaUnsatisfiable, false, true, ""},
		{ClassHostFailure, false, true, ""},
		{ClassModelFailure, false, true, ""},
		{ClassDestroyUnconfirmed, false, true, ""},
		{ClassWiring, false, false, "a rig that serves but could not edit a config is still a rig"},
		{ClassUnknown, false, true, "unclassified means unreasoned-about, so do not retry"},
	}
	for _, c := range cases {
		if got := c.class.Retryable(); got != c.retry {
			t.Errorf("%s.Retryable() = %v, want %v  %s", c.class, got, c.retry, c.whyNot)
		}
		if got := c.class.FatalToRig(); got != c.fatal {
			t.Errorf("%s.FatalToRig() = %v, want %v  %s", c.class, got, c.fatal, c.whyNot)
		}
	}
}

func TestClassSurvivesWrapping(t *testing.T) {
	base := New(ClassProviderUnknownOutcome, "vastai.Create", errors.New("i/o timeout"))
	wrapped := fmt.Errorf("provisioning rig 01J9Z: %w", base)
	deeper := fmt.Errorf("orchestrator: %w", wrapped)

	if got := ClassOf(deeper); got != ClassProviderUnknownOutcome {
		t.Errorf("ClassOf through two wraps = %s, want provider-unknown-outcome", got)
	}
	if Retryable(deeper) {
		t.Error("a wrapped unknown-outcome must still refuse retry")
	}
	if !errors.Is(deeper, base) {
		t.Error("errors.Is should reach the classified error")
	}
}

func TestUnclassifiedIsNotRetryable(t *testing.T) {
	if Retryable(errors.New("something went wrong")) {
		t.Error("an unclassified error must not be retryable")
	}
	if got := ClassOf(nil); got != ClassUnknown {
		t.Errorf("ClassOf(nil) = %s, want unknown", got)
	}
}

func TestErrorMessageNamesOpAndClass(t *testing.T) {
	e := Newf(ClassHostFailure, "runtime.Bootstrap", "sshd never came up after %s", "5m")
	want := "runtime.Bootstrap: host-failure: sshd never came up after 5m"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
