// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package workflow

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source is where one asset's bytes come from.
//
// A ComfyUI graph names files and not origins, so this is the piece the graph
// cannot supply and somebody has to. It is deliberately a repository and a
// path rather than a bare URL: the size can be read from the repository's
// listing before anything is rented, and a revision can be pinned, neither of
// which an opaque URL offers.
type Source struct {
	Repo     string `yaml:"repo" json:"repo"`
	File     string `yaml:"file" json:"file"`
	Revision string `yaml:"revision,omitempty" json:"revision,omitempty"`

	// Bytes overrides the looked-up size. It exists for sources that cannot
	// be measured ahead of time and for tests; a manifest that sets it is
	// asserting a number rather than discovering one.
	Bytes uint64 `yaml:"bytes,omitempty" json:"bytes,omitempty"`
}

// Manifest maps the filenames a graph uses to the places they come from.
//
// It is a separate file rather than an annotation inside the workflow because
// the workflow is the operator's artefact and is shared, re-exported, and
// rewritten by ComfyUI itself on every save — anything LARRI added to it would
// not survive. The manifest is LARRI's and persists.
type Manifest struct {
	Models map[string]Source `yaml:"models" json:"models"`
}

// LoadManifest reads a manifest file.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("workflow: manifest %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("workflow: manifest %s: %w", path, err)
	}
	for name, src := range m.Models {
		if src.Repo == "" {
			return nil, fmt.Errorf("workflow: manifest %s: %s has no repo", path, name)
		}
		if src.File == "" {
			// The common case is that the file is called what the graph
			// calls it, so an entry may name only the repository.
			src.File = name
			m.Models[name] = src
		}
	}
	return &m, nil
}

// Lookup resolves one asset name, consulting the manifest first and the
// built-in catalogue second.
//
// That order is the one that matters: the catalogue is a convenience so the
// common graphs need no manifest at all, and it must never override what an
// operator wrote down. A catalogue entry silently winning over an explicit
// mapping would send a rig to fetch a different model than the one asked for.
func (m *Manifest) Lookup(name string) (Source, bool) {
	if m != nil {
		if src, ok := m.Models[name]; ok {
			return src, true
		}
		// A manifest may key on the bare filename while the graph refers to
		// it through a subdirectory, which is how ComfyUI itself addresses
		// models in nested folders.
		if src, ok := m.Models[baseName(name)]; ok {
			return src, true
		}
	}
	if src, ok := Catalogue[name]; ok {
		return src, true
	}
	src, ok := Catalogue[baseName(name)]
	return src, ok
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Sizer reports the size of a file in a repository, without downloading it.
//
// An interface so the resolution path is exercisable offline: measuring a
// bundle is the step that decides both the VRAM floor and the cold-start
// estimate, and a test that could not run it would leave the most
// consequential arithmetic in the package untested.
type Sizer interface {
	Size(ctx context.Context, repo, revision, file string) (uint64, error)
}

// Item is one resolved asset: what to fetch, from where, and how large.
type Item struct {
	Asset  Asset  `json:"asset"`
	Source Source `json:"source"`
	Bytes  uint64 `json:"bytes"`
}

// Bundle is everything a graph needs on the host, measured.
type Bundle struct {
	Items      []Item    `json:"items"`
	TotalBytes uint64    `json:"total_bytes"`
	Image      ImageSpec `json:"image"`
	Steps      int       `json:"steps"`
}

// ResolveOptions configures how strictly a bundle is assembled.
type ResolveOptions struct {
	// AllowPickle permits checkpoint containers that execute code on load.
	//
	// Off by default, and the default is the security position of §15: a
	// .ckpt or .pth is a pickle, torch runs it as code the moment it is
	// loaded, and it is loaded on a host that holds the operator's Hugging
	// Face token. The reason this is a flag rather than a refusal is that
	// ComfyUI's ecosystem is not vLLM's — a great many upscalers are
	// published only as .pth — so an operator who needs one can say so, once,
	// deliberately, and see in the log what they allowed.
	AllowPickle bool

	// Manifest supplies sources the catalogue does not know.
	Manifest *Manifest
}

// pickleSuffixes are containers torch deserialises by executing them.
var pickleSuffixes = []string{".ckpt", ".pt", ".pth", ".bin"}

// IsPickle reports whether a filename names a code-executing container.
func IsPickle(name string) bool {
	l := strings.ToLower(name)
	for _, s := range pickleSuffixes {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	return false
}

// Resolve turns a parsed graph into a measured bundle.
//
// Everything here happens before a create call, and that placement is the
// point (§4a). An unresolvable model, a pickle container, or a repository that
// does not answer are all reasons not to rent — and each of them, discovered
// after the rental, is discovered at a host's hourly rate with a multi-gigabyte
// download already in flight.
func Resolve(ctx context.Context, g *Graph, sz Sizer, opt ResolveOptions) (*Bundle, error) {
	b := &Bundle{Image: g.Image, Steps: g.Steps}

	var missing, pickles []string
	for _, a := range g.Distinct() {
		src, ok := opt.Manifest.Lookup(a.Name)
		if !ok {
			missing = append(missing, a.Name)
			continue
		}
		file := src.File
		if file == "" {
			file = a.Name
		}
		if IsPickle(file) && !opt.AllowPickle {
			pickles = append(pickles, a.Name)
			continue
		}
		bytes := src.Bytes
		if bytes == 0 && sz != nil {
			n, err := sz.Size(ctx, src.Repo, src.Revision, file)
			if err != nil {
				return nil, err
			}
			bytes = n
		}
		src.File = file
		b.Items = append(b.Items, Item{Asset: a, Source: src, Bytes: bytes})
		b.TotalBytes += bytes
	}

	// Pickle first. Both lists are reasons not to rent, but only one of them
	// is a security decision, and an operator reading a combined failure would
	// reach for the manifest rather than for the flag.
	if len(pickles) > 0 {
		sort.Strings(pickles)
		return nil, fmt.Errorf(
			"workflow: resolve: %s deserialise as code: safetensors required, or --allow-pickle",
			strings.Join(pickles, ", "))
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"workflow: resolve: no source for %s: add them to the model manifest",
			strings.Join(missing, ", "))
	}
	if len(b.Items) == 0 {
		return nil, fmt.Errorf("workflow: resolve: graph loads no models")
	}
	return b, nil
}

// WeightBytes is the resident model footprint the bundle implies.
//
// Every item counts. ComfyUI moves modules between host and device as a graph
// executes, so a checkpoint and a refiner are not necessarily resident at the
// same instant — but sizing for the optimistic case is how a rig OOMs on the
// one node that needed both, and the memory saved by guessing otherwise is
// never worth the rental it costs.
func (b *Bundle) WeightBytes() uint64 { return b.TotalBytes }

// Names lists what will be fetched, largest first, so a report of a long
// download leads with the file that dominates it.
func (b *Bundle) Names() []string {
	items := append([]Item(nil), b.Items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Bytes > items[j].Bytes })
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Asset.Name)
	}
	return out
}
