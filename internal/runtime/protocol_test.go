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

// The audio surface is OpenAI-compatible and answers no chat completion, so
// the two questions a caller asks about it have different answers. Collapsing
// them is how something above this issues a completion against a transcription
// server and learns about it from a 404 on a billing rig.
func TestTheAudioSurfaceIsNotAChatSurface(t *testing.T) {
	audio := runtime.ProtocolOpenAIAudio
	if audio == runtime.ProtocolOpenAI {
		t.Fatal("the audio surface reuses the chat constant")
	}
	if audio.BasePath() != "/v1" {
		t.Errorf("BasePath = %q: /v1/audio/transcriptions lives under /v1",
			audio.BasePath())
	}
	if audio.Browser() {
		t.Error("a transcription endpoint is configured into a client, not opened in a browser")
	}
	if audio.RoundTrip() == runtime.ProtocolComfyUI.RoundTrip() {
		t.Error("a transcription rig announces it is waiting for a render")
	}
}

// BasePath must be decided by the path, not by "is it chat". A binary against
// ProtocolOpenAI answers the wrong question and drops the /v1 the audio
// surface needs.
func TestBasePathFollowsTheSurfaceRatherThanTheChatQuestion(t *testing.T) {
	for _, p := range []runtime.Protocol{runtime.ProtocolOpenAI, runtime.ProtocolOpenAIAudio} {
		if got := p.BasePath(); got != "/v1" {
			t.Errorf("%s: BasePath = %q, want /v1", p, got)
		}
	}
	if got := runtime.ProtocolComfyUI.BasePath(); got != "/" {
		t.Errorf("comfyui: BasePath = %q, want /", got)
	}
	// An unknown protocol must not be published under /v1, which would
	// advertise a contract nobody promised.
	if got := runtime.Protocol("something-later").BasePath(); got != "/" {
		t.Errorf("an unknown protocol was published at %q", got)
	}
}

// Every protocol names its own exchange, and none of them may share a name:
// the whole point is that the operator is told what is actually happening.
func TestEachProtocolNamesItsOwnRoundTrip(t *testing.T) {
	seen := map[string]runtime.Protocol{}
	for _, p := range []runtime.Protocol{
		runtime.ProtocolOpenAI, runtime.ProtocolOpenAIAudio, runtime.ProtocolComfyUI,
	} {
		name := p.RoundTrip()
		if name == "" {
			t.Errorf("%s names no round trip", p)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%s and %s both call it %q", p, other, name)
		}
		seen[name] = p
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

// The guard is about chat, so an audio server must not satisfy it however
// OpenAI-compatible it is.
func TestRequireOpenAIRefusesTheAudioSurface(t *testing.T) {
	if runtime.RequireOpenAI(protocolOnly{p: runtime.ProtocolOpenAIAudio}) {
		t.Error("a transcription server was accepted as a completion endpoint")
	}
}

// protocolOnly answers its protocol and nothing else, which is all
// RequireOpenAI asks. The embedded nil is deliberate: a call to anything else
// should panic rather than quietly return a zero value.
type protocolOnly struct {
	runtime.Workload
	p runtime.Protocol
}

func (o protocolOnly) Protocol() runtime.Protocol { return o.p }
