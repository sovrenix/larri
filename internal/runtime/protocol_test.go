// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package runtime_test

import (
	"testing"

	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/runtime/llamacpp"
	"go.sovrenix.com/larri/internal/runtime/ollama"
	"go.sovrenix.com/larri/internal/runtime/vllm"
)

// An external test package, so the engines can be imported without the cycle
// their own dependency on runtime would otherwise create.

// Runtime is a promise about the wire format, and P2 rests on it: the wiring,
// the chat UI and the IDE configuration all assume /v1 is there. Widening the
// abstraction to admit ComfyUI made that promise checkable rather than
// assumed, and this is the check.
func TestEveryRuntimeServesOpenAI(t *testing.T) {
	engines := map[string]runtime.Runtime{
		"vllm":     vllm.New(),
		"llamacpp": llamacpp.New(),
		"ollama":   ollama.New(),
		"fake":     fake.New(fake.Behaviour{}),
	}
	for name, r := range engines {
		if got := r.Protocol(); got != runtime.ProtocolOpenAI {
			t.Errorf("%s: protocol = %q, want %q — a Runtime that does not "+
				"serve /v1 breaks every client wired against it",
				name, got, runtime.ProtocolOpenAI)
		}
		if r.Protocol().Browser() {
			t.Errorf("%s: an inference engine is configured into a client, "+
				"not opened in a browser", name)
		}
	}
}

// Every Runtime is a Workload. The reverse must not hold, which is what lets
// the whole rental lifecycle be reused by something that serves no /v1 at all.
func TestRuntimesAreWorkloads(t *testing.T) {
	var _ runtime.Workload = vllm.New()
	var _ runtime.Workload = llamacpp.New()
	var _ runtime.Workload = ollama.New()
	var _ runtime.Workload = fake.New(fake.Behaviour{})

	if runtime.ProtocolOpenAI.Browser() {
		t.Error("the /v1 surface was marked as a browser surface")
	}
	if !runtime.ProtocolComfyUI.Browser() {
		t.Error("comfyui is opened in a browser and must be marked as such")
	}
}

func TestRequireOpenAIGuardsCompletionCallers(t *testing.T) {
	if !runtime.RequireOpenAI(vllm.New()) {
		t.Error("vllm was not recognised as an openai surface")
	}
	if runtime.RequireOpenAI(nil) {
		t.Error("a nil workload was reported as serving /v1")
	}
}
