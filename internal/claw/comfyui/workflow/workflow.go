// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package workflow reads a ComfyUI graph and reports what hardware it needs.
//
// It sits under the comfyui claw rather than beside it because nothing else
// will ever want it: a ComfyUI graph is not a format any other application
// reads. Kept a package of its own so the parsing is testable without a claw,
// a registry, or a rented GPU anywhere in the picture.
//
// It exists so the expensive questions are answered before the money. A
// ComfyUI workflow names its models as bare filenames —
// "sd_xl_base_1.0.safetensors" — and says nothing about how large they are,
// where they come from, or whether the card about to be rented can hold them.
// Every one of those is knowable locally, and §4a is unambiguous about what
// follows: a precondition that can be established without renting must be.
//
// The package answers three questions and stops. Which files does this graph
// load, how large is the image it is being asked to make, and how many
// sampling steps. Turning that into VRAM is sizing's job (invariant 5), and
// turning it into bytes-to-download is the manifest's.
package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Kind is the directory under ComfyUI's models/ tree that an asset belongs
// in. It is part of the download plan rather than a label: a checkpoint
// written into models/loras is a file ComfyUI will never find.
type Kind string

const (
	KindCheckpoint   Kind = "checkpoints"
	KindLoRA         Kind = "loras"
	KindVAE          Kind = "vae"
	KindControlNet   Kind = "controlnet"
	KindCLIP         Kind = "text_encoders"
	KindCLIPVision   Kind = "clip_vision"
	KindUNet         Kind = "diffusion_models"
	KindUpscale      Kind = "upscale_models"
	KindStyle        Kind = "style_models"
	KindGLIGEN       Kind = "gligen"
	KindHypernetwork Kind = "hypernetworks"
	KindPhotoMaker   Kind = "photomaker"
)

// loaders maps a node class to the directory its file lives in and the input
// fields that name one.
//
// A table rather than a heuristic, because the two serialisations need
// different things from it. The API format names its inputs, so the fields are
// read directly; the UI format stores widget values as a bare positional
// array, so only the class is available and the value has to be recognised by
// shape. Both paths need the class-to-directory mapping, and having it once is
// what keeps them from disagreeing about where a file goes.
var loaders = map[string]struct {
	Kind   Kind
	Fields []string
}{
	"CheckpointLoaderSimple":    {KindCheckpoint, []string{"ckpt_name"}},
	"CheckpointLoader":          {KindCheckpoint, []string{"ckpt_name"}},
	"ImageOnlyCheckpointLoader": {KindCheckpoint, []string{"ckpt_name"}},
	"unCLIPCheckpointLoader":    {KindCheckpoint, []string{"ckpt_name"}},
	"LoraLoader":                {KindLoRA, []string{"lora_name"}},
	"LoraLoaderModelOnly":       {KindLoRA, []string{"lora_name"}},
	"VAELoader":                 {KindVAE, []string{"vae_name"}},
	"ControlNetLoader":          {KindControlNet, []string{"control_net_name"}},
	"DiffControlNetLoader":      {KindControlNet, []string{"control_net_name"}},
	"CLIPLoader":                {KindCLIP, []string{"clip_name"}},
	"DualCLIPLoader":            {KindCLIP, []string{"clip_name1", "clip_name2"}},
	"TripleCLIPLoader":          {KindCLIP, []string{"clip_name1", "clip_name2", "clip_name3"}},
	"CLIPVisionLoader":          {KindCLIPVision, []string{"clip_name"}},
	"UNETLoader":                {KindUNet, []string{"unet_name"}},
	"UpscaleModelLoader":        {KindUpscale, []string{"model_name"}},
	"StyleModelLoader":          {KindStyle, []string{"style_model_name"}},
	"GLIGENLoader":              {KindGLIGEN, []string{"gligen_name"}},
	"HypernetworkLoader":        {KindHypernetwork, []string{"hypernetwork_name"}},
	"PhotoMakerLoader":          {KindPhotoMaker, []string{"photomaker_model_name"}},
}

// KindOf reports the models/ directory a node class loads from.
func KindOf(class string) (Kind, bool) {
	l, ok := loaders[class]
	if !ok {
		return "", false
	}
	return l.Kind, true
}

// Asset is one model file a graph loads.
type Asset struct {
	NodeID string `json:"node_id"`
	Class  string `json:"class"`
	Field  string `json:"field,omitempty"` // empty for UI-format graphs
	Name   string `json:"name"`            // as ComfyUI resolves it, subdirectories included
	Kind   Kind   `json:"kind"`
}

// Format is which of ComfyUI's two serialisations a graph is written in.
//
// The distinction has teeth and is not a detail of parsing. ComfyUI's /prompt
// endpoint accepts the API format and only the API format; the UI format is
// what the browser's save button writes, and converting between them needs
// each node's declared input order, which only a *running* ComfyUI can supply
// from /object_info. So a UI-format graph can be planned against — the models
// and the resolution are both readable — and cannot be submitted, and saying
// which is which up front beats failing at the point of execution on a host
// that is already billing.
type Format string

const (
	FormatAPI Format = "api"
	FormatUI  Format = "ui"
)

// Executable reports whether ComfyUI can be handed this graph directly.
func (f Format) Executable() bool { return f == FormatAPI }

// Graph is a parsed ComfyUI workflow.
type Graph struct {
	Format Format          `json:"format"`
	Assets []Asset         `json:"assets"`
	Image  ImageSpec       `json:"image"`
	Steps  int             `json:"steps"`
	Nodes  int             `json:"nodes"`
	Raw    json.RawMessage `json:"-"` // the bytes as given, for submission unchanged
}

// ImageSpec is the latent the graph asks for, which is the term that scales
// activation memory: a 1024x1024 render is four times the latent of 512x512,
// and the VAE decode at the end of it is the peak of the whole run.
type ImageSpec struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	Batch  int `json:"batch"`
}

// Pixels is width x height x batch, the quantity activation memory tracks.
func (s ImageSpec) Pixels() int {
	b := s.Batch
	if b < 1 {
		b = 1
	}
	return s.Width * s.Height * b
}

// Parse reads a workflow in either serialisation.
//
// Detection is structural rather than by filename: a UI graph has a "nodes"
// array, an API graph is an object of nodes each carrying a class_type. A file
// that is neither is rejected here, locally, where being wrong is free.
func Parse(b []byte) (*Graph, error) {
	var probe struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	var g *Graph
	var err error
	if e := json.Unmarshal(b, &probe); e == nil && len(probe.Nodes) > 0 {
		g, err = parseUI(b)
	} else {
		g, err = parseAPI(b)
	}
	if err != nil {
		return nil, err
	}
	g.Raw = append(json.RawMessage(nil), b...)
	return g, nil
}

// apiNode is one entry of the API serialisation.
type apiNode struct {
	ClassType string                     `json:"class_type"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
}

func parseAPI(b []byte) (*Graph, error) {
	var nodes map[string]apiNode
	if err := json.Unmarshal(b, &nodes); err != nil {
		return nil, fmt.Errorf("workflow: parse: not a comfyui graph: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("workflow: parse: graph has no nodes")
	}

	g := &Graph{Format: FormatAPI, Nodes: len(nodes)}
	var classed int
	// Sorted, so the asset order — and therefore the download order and every
	// message about it — is the same on every run.
	for _, id := range sortedNodeKeys(nodes) {
		n := nodes[id]
		if n.ClassType == "" {
			continue
		}
		classed++
		if l, ok := loaders[n.ClassType]; ok {
			for _, field := range l.Fields {
				name, ok := stringInput(n.Inputs[field])
				if !ok || name == "" {
					continue
				}
				g.Assets = append(g.Assets, Asset{
					NodeID: id, Class: n.ClassType, Field: field,
					Name: name, Kind: l.Kind,
				})
			}
		}
		g.readLatent(n)
		g.readSteps(n)
	}
	if classed == 0 {
		return nil, fmt.Errorf("workflow: parse: no node declares a class_type")
	}
	return g, nil
}

// readLatent picks up the render size from whichever node declares one.
func (g *Graph) readLatent(n apiNode) {
	switch n.ClassType {
	case "EmptyLatentImage", "EmptySD3LatentImage", "EmptyLatentImagePresets":
		w, _ := intInput(n.Inputs["width"])
		h, _ := intInput(n.Inputs["height"])
		batch, _ := intInput(n.Inputs["batch_size"])
		g.Image.absorb(ImageSpec{Width: w, Height: h, Batch: batch})
	}
}

// readSteps picks up the sampler's step count, which is billed time rather
// than memory: it multiplies how long a render takes and nothing else.
func (g *Graph) readSteps(n apiNode) {
	switch n.ClassType {
	case "KSampler", "KSamplerAdvanced", "SamplerCustom":
		if s, ok := intInput(n.Inputs["steps"]); ok && s > g.Steps {
			g.Steps = s
		}
	}
}

// absorb keeps the largest latent seen. A graph with several is sized for the
// biggest, since that is the one that has to fit.
func (s *ImageSpec) absorb(o ImageSpec) {
	if o.Width*o.Height > s.Width*s.Height {
		s.Width, s.Height = o.Width, o.Height
	}
	if o.Batch > s.Batch {
		s.Batch = o.Batch
	}
}

// uiNode is one entry of the browser's serialisation.
type uiNode struct {
	ID            json.RawMessage `json:"id"`
	Type          string          `json:"type"`
	WidgetsValues json.RawMessage `json:"widgets_values"`
}

func parseUI(b []byte) (*Graph, error) {
	var doc struct {
		Nodes []uiNode `json:"nodes"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("workflow: parse: not a comfyui ui graph: %w", err)
	}
	g := &Graph{Format: FormatUI, Nodes: len(doc.Nodes)}
	for _, n := range doc.Nodes {
		id := strings.Trim(string(n.ID), `"`)
		vals := widgetValues(n.WidgetsValues)
		if l, ok := loaders[n.Type]; ok {
			// Positional widget arrays carry no field names, so the file is
			// recognised by shape. That is looser than the API path, and it
			// is the looseness the format forces: the alternative is a
			// per-class index table that breaks silently the moment a node
			// gains a widget.
			for _, v := range vals {
				if s, ok := v.(string); ok && LooksLikeModelFile(s) {
					g.Assets = append(g.Assets, Asset{
						NodeID: id, Class: n.Type, Name: s, Kind: l.Kind,
					})
				}
			}
		}
		switch n.Type {
		case "EmptyLatentImage", "EmptySD3LatentImage":
			w, h, batch := firstThreeInts(vals)
			g.Image.absorb(ImageSpec{Width: w, Height: h, Batch: batch})
		case "KSampler", "KSamplerAdvanced":
			g.absorbUISteps(vals)
		}
	}
	return g, nil
}

// absorbUISteps reads a sampler's step count out of a positional widget array.
//
// The first plausible integer is taken: in every stock sampler the step count
// leads, and the values that follow it are a seed (too large), a cfg scale
// (usually fractional), or a name. Bounded above, so a seed that happens to be
// small is not mistaken for two thousand sampling steps.
func (g *Graph) absorbUISteps(vals []any) {
	for _, v := range vals {
		f, ok := v.(float64)
		if !ok || f <= 0 || f != float64(int(f)) {
			continue
		}
		if n := int(f); n <= maxPlausibleSteps && n > g.Steps {
			g.Steps = n
			return
		}
	}
}

// maxPlausibleSteps bounds what a positional read will accept as a step count.
const maxPlausibleSteps = 200

// modelSuffixes are the container extensions ComfyUI loads.
var modelSuffixes = []string{
	".safetensors", ".sft", ".ckpt", ".pt", ".pth", ".bin", ".gguf", ".onnx",
}

// LooksLikeModelFile reports whether a widget value names a weight file.
//
// Exported because the manifest applies the same test when deciding whether an
// unresolved name is a file it should have been told about, or simply a
// sampler name it has no business fetching.
func LooksLikeModelFile(s string) bool {
	l := strings.ToLower(s)
	for _, suf := range modelSuffixes {
		if strings.HasSuffix(l, suf) {
			return true
		}
	}
	return false
}

// Names returns the distinct asset filenames, in a stable order.
func (g *Graph) Names() []string {
	var out []string
	for _, a := range g.Distinct() {
		out = append(out, a.Name)
	}
	return out
}

// Distinct collapses assets named more than once.
//
// Two LoRA nodes loading the same file are one download, and counting it twice
// would both double the estimated cold start and rank the market against a
// size that does not exist.
func (g *Graph) Distinct() []Asset {
	seen := map[string]bool{}
	var out []Asset
	for _, a := range g.Assets {
		if seen[a.Name] {
			continue
		}
		seen[a.Name] = true
		out = append(out, a)
	}
	return out
}

func stringInput(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// A link — ["3", 0] — rather than a literal. The file is named at
		// the node that produces it, which this walk reaches on its own.
		return "", false
	}
	return s, true
}

func intInput(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return int(f), true
}

func widgetValues(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	// Newer graphs sometimes store widgets as an object keyed by name.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		out := make([]any, 0, len(obj))
		for _, k := range sortedAnyKeys(obj) {
			out = append(out, obj[k])
		}
		return out
	}
	return nil
}

func firstThreeInts(vals []any) (w, h, batch int) {
	var got []int
	for _, v := range vals {
		if f, ok := v.(float64); ok {
			got = append(got, int(f))
		}
	}
	for i, v := range got {
		switch i {
		case 0:
			w = v
		case 1:
			h = v
		case 2:
			batch = v
		}
	}
	return w, h, batch
}

func sortedNodeKeys(m map[string]apiNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
