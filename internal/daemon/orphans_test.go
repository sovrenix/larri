// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"strings"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
)

// orphanFixture brings a rig up, forces it into the given state, and leaves
// the provider still reporting the instance as live.
func orphanFixture(t *testing.T, state core.LifecycleState) (*Orchestrator, string) {
	t.Helper()
	o, _, st := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	rig, err := o.Up(context.Background(), upReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(rig, state, "test"); err != nil {
		t.Fatal(err)
	}
	return o, rig.Instance.InstanceID
}

// A live bring-up was listed as an orphan, with the reason "tracked as
// BOOTSTRAPPING" printed directly beneath the heading "local state does not
// account for" — a contradiction the output stated on consecutive lines. The
// consequence was not cosmetic: `larri orphans --destroy` offered to destroy
// a rig that had been running for half an hour, and answering yes would have
// killed it.
func TestATrackedRigInFlightIsNotAnOrphan(t *testing.T) {
	for _, state := range []core.LifecycleState{
		core.StateProvisioned, core.StateBootstrapping, core.StateReady, core.StateDegraded,
	} {
		t.Run(string(state), func(t *testing.T) {
			o, inst := orphanFixture(t, state)
			got, err := o.Orphans(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, orph := range got {
				if orph.Instance.InstanceID == inst {
					t.Errorf("a rig tracked as %s was offered for destruction: %s",
						state, orph.Reason)
				}
			}
		})
	}
}

// The inconsistencies that ARE orphans must still be caught: billing that
// local state says should not be happening is the whole point of the sweep.
func TestInconsistentStateIsStillAnOrphan(t *testing.T) {
	for _, tc := range []struct {
		state core.LifecycleState
		want  string
	}{
		{core.StateDestroyed, "still here"},
		{core.StateFailed, "no resource behind it"},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			o, inst := orphanFixture(t, tc.state)
			got, err := o.Orphans(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var found *Orphan
			for i := range got {
				if got[i].Instance.InstanceID == inst {
					found = &got[i]
				}
			}
			if found == nil {
				t.Fatalf("a rig tracked as %s while still billing must be surfaced", tc.state)
			}
			if !strings.Contains(found.Reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", found.Reason, tc.want)
			}
		})
	}
}
