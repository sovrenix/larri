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
