// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"testing"

	"go.sovrenix.com/larri/internal/core"
	pfake "go.sovrenix.com/larri/internal/provider/fake"
	"go.sovrenix.com/larri/internal/runtime"
	rfake "go.sovrenix.com/larri/internal/runtime/fake"
	"go.sovrenix.com/larri/internal/sizing"
)

// fileEngine picks one file per quantisation, the way llama.cpp does, and
// defaults to Q4_K_M when asked for none.
type fileEngine struct {
	*rfake.Runtime
	files  map[string]runtime.Weights
	chosen runtime.Weights
	asked  []string
}

func (e *fileEngine) DefaultQuantization() string { return "Q4_K_M" }

func (e *fileEngine) ResolveWeights(_ context.Context, spec core.ModelSpec) (runtime.Weights, error) {
	q := spec.Quantization
	if q == "" {
		q = e.DefaultQuantization()
	}
	e.asked = append(e.asked, spec.Quantization)
	e.chosen = e.files[q]
	return e.chosen, nil
}

func (e *fileEngine) WeightBytes() uint64 { return e.chosen.Bytes }

func newFileEngine() *fileEngine {
	return &fileEngine{
		Runtime: rfake.New(rfake.Behaviour{}),
		files: map[string]runtime.Weights{
			"Q4_K_M": {File: "m-Q4_K_M.gguf", Bytes: 5 << 30, Quantization: "Q4_K_M"},
			"BF16":   {File: "m-BF16.gguf", Bytes: 16 << 30, Quantization: "BF16"},
		},
	}
}

// Every surface reaches sizing through the daemon, so the daemon is where
// the engine's default has to be settled. The CLI settled it after the file
// was already chosen, and the other surfaces filled in fp16 themselves.
func TestAnUnnamedQuantizationIsSizedAtTheEnginesDefault(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	eng := newFileEngine()
	o.Runtime = eng
	req := upReq()
	req.Model.Quantization = ""

	sv, err := o.Offers(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.asked) != 1 || eng.asked[0] != "" {
		t.Errorf("engine was asked for %q; an unnamed quantisation is the engine's to default", eng.asked)
	}
	if sv.Model.Quantization != "Q4_K_M" {
		t.Errorf("survey carries quantization %q, want Q4_K_M", sv.Model.Quantization)
	}
	if sv.Plan.WeightsBytes != 5<<30 {
		t.Errorf("sized %s of weights; the Q4_K_M file is 5.0 GB", sizing.HumanBytes(sv.Plan.WeightsBytes))
	}
}

// The rig records what is being served, so status names the file's
// quantisation rather than the empty one the request carried.
func TestTheRigRecordsTheResolvedQuantization(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	o.Runtime = newFileEngine()
	req := upReq()
	req.Model.Quantization = ""

	rig, err := o.Up(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if rig.Model.Quantization != "Q4_K_M" {
		t.Errorf("rig recorded quantization %q, want Q4_K_M", rig.Model.Quantization)
	}
}

// An engine with no default of its own serves full precision, which is what
// every surface used to fill in before sending the request.
func TestAnEngineWithoutADefaultIsSizedAtFullPrecision(t *testing.T) {
	o, _, _ := newOrch(t, pfake.Behaviour{}, rfake.Behaviour{})
	req := upReq()
	req.Model.Quantization = ""

	sv, err := o.Offers(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sv.Model.Quantization != "fp16" {
		t.Errorf("quantization = %q, want fp16", sv.Model.Quantization)
	}
}
