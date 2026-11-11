// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package core

import "testing"

// The answer decides whether `larri down` will destroy a host that holds the
// only copy of what the operator made. A live run lost a render to exactly
// that, so the interesting assertion is the default: a kind nobody has
// classified must not read as an inference engine.
func TestServesInferenceProtectsAnythingItDoesNotKnow(t *testing.T) {
	for _, k := range []RuntimeKind{RuntimeVLLM, RuntimeLlamaCpp, RuntimeOllama} {
		if !k.ServesInference() {
			t.Errorf("%s is an inference engine and does not say so", k)
		}
	}
	for _, k := range []RuntimeKind{RuntimeComfyUI, "", "some-claw-added-later"} {
		if k.ServesInference() {
			t.Errorf("%q claims to serve inference, so a teardown would not "+
				"stop to ask about its results", k)
		}
	}
}

// ServesInference answers "is this an engine", which is not the question a
// teardown has. A local claw is not an engine and holds nothing on the host,
// and asking the first question to answer the second refused every live
// speech-to-text rig with an instruction to collect files that were never
// there.
func TestOnlyARemotePayloadHoldsResultsOnTheHost(t *testing.T) {
	cases := []struct {
		name string
		rig  Rig
		want bool
	}{
		{"an engine keeps nothing of the operator's",
			Rig{Runtime: RuntimeVLLM}, false},
		{"an engine is not guarded by a stray site either",
			Rig{Runtime: RuntimeLlamaCpp, ClawSite: ClawSiteRemote}, false},
		{"a remote claw's renders exist only there",
			Rig{Runtime: RuntimeComfyUI, ClawSite: ClawSiteRemote}, true},
		{"a local claw wrote its output on this machine",
			Rig{Runtime: RuntimeWhisper, ClawSite: ClawSiteLocal}, false},
		// The default is the load-bearing one: a claw type added later, or a
		// record written before the site was persisted, must be protected
		// before anyone remembers this function exists.
		{"an unrecorded site reads as remote",
			Rig{Runtime: "some-claw-added-later"}, true},
	}
	for _, c := range cases {
		if got := c.rig.HoldsHostResults(); got != c.want {
			t.Errorf("%s: HoldsHostResults() = %v, want %v", c.name, got, c.want)
		}
	}
}
