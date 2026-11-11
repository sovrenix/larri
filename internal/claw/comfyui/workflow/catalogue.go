// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package workflow

// Catalogue maps the filenames ComfyUI's own documentation uses to the
// repositories they are published from.
//
// It exists so the graphs an operator is most likely to try need no manifest
// at all, and it is deliberately short. Every entry is a claim about a third
// party's repository that this file cannot verify, so the catalogue is built
// to fail safely rather than to be exhaustive: a wrong or stale entry is
// caught by the size lookup returning "not found" during resolution, which
// happens locally, before a create call, and costs nothing (§4a). Breadth
// belongs in an operator's manifest, which Lookup consults first.
//
// Filenames are the keys because that is what a graph carries. Two different
// repositories publishing the same filename is exactly the ambiguity a
// manifest exists to settle.
var Catalogue = map[string]Source{
	// SDXL. The base checkpoint and its refiner are the pair most stock
	// graphs open with, and the offset LoRA ships alongside the base.
	"sd_xl_base_1.0.safetensors": {
		Repo: "stabilityai/stable-diffusion-xl-base-1.0",
		File: "sd_xl_base_1.0.safetensors",
	},
	"sd_xl_refiner_1.0.safetensors": {
		Repo: "stabilityai/stable-diffusion-xl-refiner-1.0",
		File: "sd_xl_refiner_1.0.safetensors",
	},
	"sd_xl_offset_example-lora_1.0.safetensors": {
		Repo: "stabilityai/stable-diffusion-xl-base-1.0",
		File: "sd_xl_offset_example-lora_1.0.safetensors",
	},
	"sdxl_vae.safetensors": {
		Repo: "stabilityai/sdxl-vae",
		File: "sdxl_vae.safetensors",
	},

	// FLUX. The dev weights are gated, so resolution exercises HF_TOKEN
	// during sizing rather than forty minutes into a bootstrap — the same
	// property the LLM path relies on.
	"flux1-schnell.safetensors": {
		Repo: "black-forest-labs/FLUX.1-schnell",
		File: "flux1-schnell.safetensors",
	},
	"flux1-dev.safetensors": {
		Repo: "black-forest-labs/FLUX.1-dev",
		File: "flux1-dev.safetensors",
	},
	"ae.safetensors": {
		Repo: "black-forest-labs/FLUX.1-schnell",
		File: "ae.safetensors",
	},

	// FLUX text encoders, published separately from the transformer because
	// ComfyUI loads them as their own nodes.
	"clip_l.safetensors": {
		Repo: "comfyanonymous/flux_text_encoders",
		File: "clip_l.safetensors",
	},
	"t5xxl_fp16.safetensors": {
		Repo: "comfyanonymous/flux_text_encoders",
		File: "t5xxl_fp16.safetensors",
	},
	"t5xxl_fp8_e4m3fn.safetensors": {
		Repo: "comfyanonymous/flux_text_encoders",
		File: "t5xxl_fp8_e4m3fn.safetensors",
	},
}
