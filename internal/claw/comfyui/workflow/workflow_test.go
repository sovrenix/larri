// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package workflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apiGraph is a stock SDXL text-to-image graph in the serialisation /prompt
// accepts. It carries a checkpoint, a LoRA, a separate VAE, a latent and a
// sampler, which is every shape the parser has to read.
const apiGraph = `{
  "4": {"class_type": "CheckpointLoaderSimple",
        "inputs": {"ckpt_name": "sd_xl_base_1.0.safetensors"}},
  "10": {"class_type": "LoraLoader",
        "inputs": {"lora_name": "sd_xl_offset_example-lora_1.0.safetensors",
                   "strength_model": 0.8, "strength_clip": 0.8,
                   "model": ["4", 0], "clip": ["4", 1]}},
  "11": {"class_type": "VAELoader",
        "inputs": {"vae_name": "sdxl_vae.safetensors"}},
  "5": {"class_type": "EmptyLatentImage",
        "inputs": {"width": 1024, "height": 1024, "batch_size": 2}},
  "3": {"class_type": "KSampler",
        "inputs": {"steps": 25, "cfg": 8.0, "seed": 42,
                   "model": ["10", 0], "latent_image": ["5", 0]}},
  "9": {"class_type": "SaveImage", "inputs": {"images": ["8", 0]}}
}`

func TestParseAPIGraphFindsEveryLoader(t *testing.T) {
	g, err := Parse([]byte(apiGraph))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if g.Format != FormatAPI {
		t.Errorf("format = %q, want api", g.Format)
	}
	if !g.Format.Executable() {
		t.Error("an api graph must be executable; /prompt accepts nothing else")
	}
	want := map[string]Kind{
		"sd_xl_base_1.0.safetensors":                KindCheckpoint,
		"sd_xl_offset_example-lora_1.0.safetensors": KindLoRA,
		"sdxl_vae.safetensors":                      KindVAE,
	}
	if len(g.Assets) != len(want) {
		t.Fatalf("found %d assets, want %d: %+v", len(g.Assets), len(want), g.Assets)
	}
	for _, a := range g.Assets {
		kind, ok := want[a.Name]
		if !ok {
			t.Errorf("unexpected asset %q", a.Name)
			continue
		}
		if a.Kind != kind {
			t.Errorf("%s: kind = %q, want %q — a file in the wrong models/ "+
				"directory is one comfyui never finds", a.Name, a.Kind, kind)
		}
	}
}

// The latent and the step count are the two numbers that are not about which
// files to fetch: one sizes VRAM, the other sizes billed time.
func TestParseReadsLatentAndSteps(t *testing.T) {
	g, err := Parse([]byte(apiGraph))
	if err != nil {
		t.Fatal(err)
	}
	if g.Image.Width != 1024 || g.Image.Height != 1024 || g.Image.Batch != 2 {
		t.Errorf("image = %+v, want 1024x1024 batch 2", g.Image)
	}
	if got, want := g.Image.Pixels(), 1024*1024*2; got != want {
		t.Errorf("pixels = %d, want %d", got, want)
	}
	if g.Steps != 25 {
		t.Errorf("steps = %d, want 25", g.Steps)
	}
}

// A node input can be a literal or a link to another node. Reading a link as
// a filename would send the rig to fetch a model called ["4",0].
func TestLinkedInputsAreNotMistakenForFilenames(t *testing.T) {
	const linked = `{
	  "1": {"class_type": "LoraLoader", "inputs": {"lora_name": ["9", 0]}},
	  "2": {"class_type": "CheckpointLoaderSimple",
	        "inputs": {"ckpt_name": "real.safetensors"}}}`
	g, err := Parse([]byte(linked))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Assets) != 1 || g.Assets[0].Name != "real.safetensors" {
		t.Fatalf("assets = %+v, want only the literal", g.Assets)
	}
}

// Two nodes loading one file is one download. Counting it twice would double
// the cold-start estimate and rank the market against a size that does not
// exist.
func TestDistinctCollapsesRepeatedAssets(t *testing.T) {
	const twice = `{
	  "1": {"class_type": "LoraLoader", "inputs": {"lora_name": "same.safetensors"}},
	  "2": {"class_type": "LoraLoader", "inputs": {"lora_name": "same.safetensors"}}}`
	g, err := Parse([]byte(twice))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Assets) != 2 {
		t.Fatalf("assets = %d, want both occurrences recorded", len(g.Assets))
	}
	if d := g.Distinct(); len(d) != 1 {
		t.Errorf("distinct = %d, want 1", len(d))
	}
}

// The UI serialisation is what the browser's save button writes. It can be
// planned against and it cannot be submitted, and the parser must say which.
func TestParseUIGraphIsPlannableButNotExecutable(t *testing.T) {
	const ui = `{
	  "last_node_id": 9,
	  "nodes": [
	    {"id": 4, "type": "CheckpointLoaderSimple",
	     "widgets_values": ["sd_xl_base_1.0.safetensors"]},
	    {"id": 5, "type": "EmptyLatentImage", "widgets_values": [768, 768, 1]},
	    {"id": 3, "type": "KSampler", "widgets_values": [30, 8.0, "euler"]}
	  ]}`
	g, err := Parse([]byte(ui))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if g.Format != FormatUI {
		t.Fatalf("format = %q, want ui", g.Format)
	}
	if g.Format.Executable() {
		t.Error("a ui graph must not claim to be executable; /prompt rejects it")
	}
	if len(g.Assets) != 1 || g.Assets[0].Name != "sd_xl_base_1.0.safetensors" {
		t.Errorf("assets = %+v", g.Assets)
	}
	if g.Assets[0].Kind != KindCheckpoint {
		t.Errorf("kind = %q, want checkpoints", g.Assets[0].Kind)
	}
	if g.Image.Width != 768 || g.Image.Height != 768 {
		t.Errorf("image = %+v, want 768x768", g.Image)
	}
	if g.Steps != 30 {
		t.Errorf("steps = %d, want 30", g.Steps)
	}
}

// A seed is an integer sitting in the same widget array as the step count.
// Reading the wrong one would report a graph as doing two billion steps.
func TestUIStepsIgnoreImplausibleIntegers(t *testing.T) {
	const ui = `{"nodes": [
	  {"id": 3, "type": "KSampler", "widgets_values": [934857239485, 20, 8.0]}]}`
	g, err := Parse([]byte(ui))
	if err != nil {
		t.Fatal(err)
	}
	if g.Steps != 20 {
		t.Errorf("steps = %d, want 20", g.Steps)
	}
}

func TestParseRejectsNonGraphs(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"empty object", `{}`},
		{"not json", `hello`},
		{"no class types", `{"1": {"inputs": {}}}`},
	} {
		if _, err := Parse([]byte(tc.in)); err == nil {
			t.Errorf("%s: parsed without error", tc.name)
		}
	}
}

// Raw must survive parsing byte for byte: it is what gets submitted, and a
// re-serialised graph is not the graph the operator wrote.
func TestRawIsPreservedForSubmission(t *testing.T) {
	g, err := Parse([]byte(apiGraph))
	if err != nil {
		t.Fatal(err)
	}
	if string(g.Raw) != apiGraph {
		t.Error("raw graph was rewritten; /prompt must receive what the operator wrote")
	}
}

// ---- resolution -------------------------------------------------------

func testManifest() *Manifest {
	return &Manifest{Models: map[string]Source{
		"sd_xl_base_1.0.safetensors":                {Repo: "r/base", File: "base.safetensors"},
		"sd_xl_offset_example-lora_1.0.safetensors": {Repo: "r/lora", File: "lora.safetensors"},
		"sdxl_vae.safetensors":                      {Repo: "r/vae", File: "vae.safetensors"},
	}}
}

func testSizer() StaticSizer {
	return StaticSizer{
		"r/base/base.safetensors": 6_900_000_000,
		"r/lora/lora.safetensors": 50_000_000,
		"r/vae/vae.safetensors":   335_000_000,
	}
}

func TestResolveMeasuresTheWholeBundle(t *testing.T) {
	g, err := Parse([]byte(apiGraph))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Resolve(context.Background(), g, testSizer(),
		ResolveOptions{Manifest: testManifest()})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const want = 6_900_000_000 + 50_000_000 + 335_000_000
	if b.TotalBytes != want {
		t.Errorf("total = %d, want %d", b.TotalBytes, want)
	}
	if len(b.Items) != 3 {
		t.Errorf("items = %d, want 3", len(b.Items))
	}
	if b.Image.Width != 1024 {
		t.Errorf("image did not survive resolution: %+v", b.Image)
	}
	// Largest first, so a report of a long download leads with the file that
	// dominates it.
	if names := b.Names(); names[0] != "sd_xl_base_1.0.safetensors" {
		t.Errorf("names = %v, want the checkpoint first", names)
	}
}

// An unresolvable model is a reason not to rent, and the message has to name
// the files so the operator knows what to add.
//
// The names are deliberately ones the built-in catalogue does not carry. An
// earlier version of this test used the stock SDXL filenames and passed for
// the wrong reason: the catalogue resolved them, and what failed was the size
// lookup rather than the lookup of a source.
func TestResolveRefusesUnknownModels(t *testing.T) {
	const unknown = `{
	  "1": {"class_type": "CheckpointLoaderSimple",
	        "inputs": {"ckpt_name": "somebodys_merge_v3.safetensors"}},
	  "2": {"class_type": "LoraLoader",
	        "inputs": {"lora_name": "private_style.safetensors"}}}`
	g, err := Parse([]byte(unknown))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(context.Background(), g, testSizer(), ResolveOptions{
		Manifest: &Manifest{Models: map[string]Source{
			"somebodys_merge_v3.safetensors": {Repo: "r/base", File: "base.safetensors"},
		}},
	})
	if err == nil {
		t.Fatal("resolved a graph whose models have no source")
	}
	if !strings.Contains(err.Error(), "private_style.safetensors") {
		t.Errorf("error does not name the missing file: %v", err)
	}
	if !strings.Contains(err.Error(), "manifest") {
		t.Errorf("error does not name the remedy: %v", err)
	}
}

// The catalogue answering for a name the operator never declared is the whole
// point of it, and a stale entry must fail locally and free rather than on a
// rented host. A source that resolves but cannot be measured is that failure.
func TestACatalogueEntryThatCannotBeMeasuredFailsBeforeSpending(t *testing.T) {
	const stock = `{"1": {"class_type": "CheckpointLoaderSimple",
	                      "inputs": {"ckpt_name": "sd_xl_base_1.0.safetensors"}}}`
	g, err := Parse([]byte(stock))
	if err != nil {
		t.Fatal(err)
	}
	// An empty sizer stands in for a repository that has moved on.
	if _, err := Resolve(context.Background(), g, StaticSizer{}, ResolveOptions{}); err == nil {
		t.Fatal("an unmeasurable catalogue entry resolved anyway")
	}
}

// §15: a .ckpt is a pickle and torch executes it on load, on the host holding
// the operator's Hugging Face token. Refusing before the rental costs nothing.
func TestResolveRefusesPickleByDefault(t *testing.T) {
	const pickled = `{"1": {"class_type": "CheckpointLoaderSimple",
	                       "inputs": {"ckpt_name": "old_model.ckpt"}}}`
	g, err := Parse([]byte(pickled))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Models: map[string]Source{
		"old_model.ckpt": {Repo: "r/old", File: "old_model.ckpt", Bytes: 2_000_000_000},
	}}
	_, err = Resolve(context.Background(), g, nil, ResolveOptions{Manifest: m})
	if err == nil {
		t.Fatal("accepted a pickle container by default")
	}
	if !strings.Contains(err.Error(), "safetensors required") {
		t.Errorf("refusal does not name the remedy: %v", err)
	}

	b, err := Resolve(context.Background(), g, nil,
		ResolveOptions{Manifest: m, AllowPickle: true})
	if err != nil {
		t.Fatalf("explicit opt-in was still refused: %v", err)
	}
	if b.TotalBytes != 2_000_000_000 {
		t.Errorf("total = %d", b.TotalBytes)
	}
}

// The catalogue is a convenience and must never beat what an operator wrote
// down: a silent override would fetch a different model than the one asked for.
func TestManifestBeatsCatalogue(t *testing.T) {
	m := &Manifest{Models: map[string]Source{
		"sd_xl_base_1.0.safetensors": {Repo: "mine/fork", File: "fork.safetensors"},
	}}
	src, ok := m.Lookup("sd_xl_base_1.0.safetensors")
	if !ok {
		t.Fatal("lookup failed")
	}
	if src.Repo != "mine/fork" {
		t.Errorf("repo = %q, want the manifest's entry", src.Repo)
	}
	// And with no manifest entry, the catalogue still answers.
	if src, ok := (*Manifest)(nil).Lookup("sd_xl_base_1.0.safetensors"); !ok ||
		src.Repo != "stabilityai/stable-diffusion-xl-base-1.0" {
		t.Errorf("catalogue lookup = %+v, %v", src, ok)
	}
}

// A graph may address a model through a subdirectory while the manifest keys
// on the bare name, which is how ComfyUI itself refers to nested folders.
func TestLookupFallsBackToTheBaseName(t *testing.T) {
	m := &Manifest{Models: map[string]Source{
		"vae.safetensors": {Repo: "r/vae", File: "vae.safetensors"},
	}}
	if _, ok := m.Lookup("sdxl/vae.safetensors"); !ok {
		t.Error("a nested reference did not fall back to the base name")
	}
}

func TestLoadManifestDefaultsFileToTheKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(path, []byte(
		"models:\n  thing.safetensors:\n    repo: org/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	src := m.Models["thing.safetensors"]
	if src.File != "thing.safetensors" {
		t.Errorf("file = %q, want the key", src.File)
	}
}

func TestLoadManifestRejectsAnEntryWithNoRepo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(path, []byte(
		"models:\n  thing.safetensors:\n    file: x.safetensors\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err == nil {
		t.Error("accepted a manifest entry with nowhere to fetch from")
	}
}

func TestIsPickleCoversTheTorchContainers(t *testing.T) {
	for _, name := range []string{"a.ckpt", "b.pt", "c.pth", "d.bin", "E.CKPT"} {
		if !IsPickle(name) {
			t.Errorf("%s not recognised as a pickle container", name)
		}
	}
	for _, name := range []string{"a.safetensors", "b.gguf", "c.sft"} {
		if IsPickle(name) {
			t.Errorf("%s wrongly refused", name)
		}
	}
}

// A filename is not unique across a graph. ComfyUI resolves each reference
// under the models/ subdirectory its node implies, so the same name can be a
// checkpoint and a LoRA at once — two files, two directories, two downloads.
// Keyed on the name alone they collapsed to one, and the node whose copy was
// dropped found nothing on a rig that was already billing.
func TestOneNameUnderTwoKindsIsTwoAssets(t *testing.T) {
	g := &Graph{Assets: []Asset{
		{Name: "shared.safetensors", Kind: KindCheckpoint, NodeID: "1"},
		{Name: "shared.safetensors", Kind: KindLoRA, NodeID: "2"},
		// And a genuine repeat, which must still collapse: two LoRA nodes
		// loading one file are one download.
		{Name: "shared.safetensors", Kind: KindLoRA, NodeID: "3"},
	}}
	got := g.Distinct()
	if len(got) != 2 {
		t.Fatalf("Distinct() kept %d of 3 assets, want 2: %+v", len(got), got)
	}
	kinds := map[Kind]bool{}
	for _, a := range got {
		kinds[a.Kind] = true
	}
	if !kinds[KindCheckpoint] || !kinds[KindLoRA] {
		t.Errorf("kept %v; one directory's copy was dropped", kinds)
	}
	if a, b := got[0].Key(), got[1].Key(); a == b {
		t.Errorf("both assets key to %q, so one url overwrites the other", a)
	}
}

// Hugging Face reports no size for a file whose LFS pointer it has not
// resolved. Taking that at face value planned the model at nothing — VRAM and
// disk understated, and the fetch script's own check skipped, since it only
// verifies a positive expectation. Zero is worse than an estimate: it is an
// estimate that always fits.
func TestAListingWithNoSizeIsNotAModelOfZeroBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"siblings":[
			{"rfilename":"sized.safetensors","size":1234},
			{"rfilename":"unsized.safetensors","size":0}]}`))
	}))
	defer srv.Close()

	h := &HFSizer{Endpoint: srv.URL, repos: map[string]map[string]uint64{}}
	if n, err := h.Size(context.Background(), "r/m", "main", "sized.safetensors"); err != nil || n != 1234 {
		t.Fatalf("a measured file returned %d, %v", n, err)
	}
	if _, err := h.Size(context.Background(), "r/m", "main", "unsized.safetensors"); err == nil {
		t.Error("a file with no listed size was reported as measured at zero bytes")
	}
}
