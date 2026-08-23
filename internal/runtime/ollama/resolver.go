// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package ollama

import (
	"context"

	"go.sovrenix.com/larri/internal/sizing"
)

// Resolver sizes Ollama tags from the registry.
//
// Hugging Face's resolver cannot: an Ollama tag is not a Hugging Face
// repository, and asking for one returns a 404 that reads like a permissions
// problem. Facts come instead from the model's own GGUF header, which is a
// better source than a config.json — it describes the exact artefact that will
// be served, quantisation included.
type Resolver struct{}

// Resolve returns architecture facts for an Ollama tag.
func (Resolver) Resolve(ctx context.Context, ref, _ string) (sizing.Facts, error) {
	info, err := Inspect(ctx, ref)
	if err != nil {
		return sizing.Facts{}, err
	}
	return info.Facts(), nil
}

// Facts renders an inspection as sizing facts.
func (i Info) Facts() sizing.Facts {
	return sizing.Facts{
		Ref:           i.Ref,
		Params:        i.Params(),
		Layers:        i.Layers,
		KVHeads:       i.KVHeads,
		HeadDim:       i.HeadDim,
		HiddenSize:    i.HiddenSize,
		MaxContextLen: i.MaxContextLen,
	}
}

var _ sizing.Resolver = Resolver{}
