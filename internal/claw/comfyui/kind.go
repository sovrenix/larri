// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/claw/comfyui/workflow"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/sizing"
)

// Type is the name the operator passes to --type.
const Type claw.Type = "comfyui"

func init() {
	claw.Register(Type, func() claw.Kind { return &Kind{} })
}

// Config is the shape of a comfyui job file.
//
// The workflow and the manifest are paths relative to the job file, so a job
// that works in one directory works in every other. The two pins are here
// rather than as flags because they have to move together: the image supplies
// torch and the ref supplies ComfyUI, and a pair that has never been run
// together is the failure this project has already paid for.
type Config struct {
	Workflow       string `yaml:"workflow"`
	Models         string `yaml:"models"`
	AllowPickle    bool   `yaml:"allow_pickle"`
	ComfyUIVersion string `yaml:"comfyui_version"`
	Image          string `yaml:"image"`
}

// Kind is the ComfyUI claw.
//
// It carries state from Plan through to Server and Collect — the parsed graph,
// the measured bundle, where each file comes from — which is why the registry
// hands out a fresh instance per run rather than a shared singleton.
type Kind struct {
	cfg    Config
	graph  *workflow.Graph
	bundle *workflow.Bundle
	urls   map[string]string
	name   string

	// sizer measures the files and says where they come from. Nil means
	// Hugging Face, which is the only source there is; a test supplies its
	// own, so the whole of §4a — parse, resolve, measure, size, choose the
	// hardware — can be exercised without a network.
	sizer Sizer
}

// Sizer resolves a bundle's files: how large each one is, and where its bytes
// come from.
type Sizer interface {
	workflow.Sizer
	DownloadURL(workflow.Source) string
}

var (
	_ claw.Kind    = (*Kind)(nil)
	_ claw.Remote  = (*Kind)(nil)
	_ claw.Browser = (*Kind)(nil)
	_ claw.Holder  = (*Kind)(nil)
)

func (k *Kind) Type() claw.Type { return Type }

// Site is remote: ComfyUI runs on the rented box, so what it renders exists
// only there and must be collected before the destroy.
func (k *Kind) Site() claw.Site { return claw.SiteRemote }

func (k *Kind) Describe() string {
	return "image-generation graphs; opens in a browser"
}

// Plan reads the job file and says what to rent, without spending anything.
//
// This is §4a applied to a graph. Every model it names is resolved to a real
// repository and measured against the live listing; the bundle plus the latent
// area become a VRAM floor and a cold-start byte count; and the market is then
// ranked on the cost of a working endpoint over the session rather than on the
// hourly rate. An unresolvable model, a pickle container, or a token that
// cannot read a gated repository each end the run here, locally, for nothing.
func (k *Kind) Plan(ctx context.Context, cfg *claw.Config, opt claw.Options) (*claw.Plan, error) {
	if err := cfg.Decode(&k.cfg); err != nil {
		return nil, err
	}
	if k.cfg.Workflow == "" {
		return nil, errs.Newf(errs.ClassModelFailure, "comfyui.Plan",
			"job file names no workflow")
	}
	wfPath := cfg.Resolve(k.cfg.Workflow)
	k.name = workflowName(wfPath)

	raw, err := os.ReadFile(wfPath)
	if err != nil {
		return nil, errs.Newf(errs.ClassModelFailure, "comfyui.Plan",
			"read workflow: %v", err)
	}
	graph, err := workflow.Parse(raw)
	if err != nil {
		return nil, err
	}
	k.graph = graph

	var manifest *workflow.Manifest
	if k.cfg.Models != "" {
		if manifest, err = workflow.LoadManifest(cfg.Resolve(k.cfg.Models)); err != nil {
			return nil, err
		}
	}
	sizer := k.sizer
	if sizer == nil {
		sizer = workflow.NewHFSizer(opt.HFToken)
	}
	bundle, err := workflow.Resolve(ctx, graph, sizer, workflow.ResolveOptions{
		Manifest: manifest, AllowPickle: k.cfg.AllowPickle,
	})
	if err != nil {
		return nil, err
	}
	k.bundle = bundle
	k.urls = make(map[string]string, len(bundle.Items))
	for _, it := range bundle.Items {
		k.urls[it.Asset.Key()] = sizer.DownloadURL(it.Source)
	}

	plan, err := sizing.PlanDiffusion(sizing.DiffusionRequest{
		WeightBytes: bundle.TotalBytes, Pixels: bundle.Image.Pixels(),
	})
	if err != nil {
		return nil, err
	}

	return &claw.Plan{
		Criteria:       k.criteria(opt.Criteria, plan),
		Model:          ModelSpecFor(k.name, graph),
		Sizing:         &plan,
		ColdStartBytes: bundle.TotalBytes,
		Summary:        k.summary(plan),
		Caveats:        k.caveats(),
	}, nil
}

// criteria turns the bundle into hardware, over whatever the operator asked
// for.
//
// The VRAM floor is **per-GPU** and not a total, which is the correction that
// matters here: ComfyUI executes a graph on one device, so two 12 GB cards do
// not hold a 20 GB bundle. Summing them would select hardware whose headroom is
// arithmetic rather than real.
func (k *Kind) criteria(base core.Criteria, plan core.SizingPlan) core.Criteria {
	need := int((plan.RequiredVRAMBytes + sizing.GiB - 1) / sizing.GiB)
	want := core.Criteria{
		// A floor, deliberately, even though PlanDiffusion computes a target
		// the engine could miss and survive. ComfyUI offloads to host RAM
		// rather than dying, so an undersized card renders the right image
		// slowly — and a slow rig is an expensive one (§4b). Refusing costs
		// the operator nothing; renting one costs them by the second.
		VRAMPerGPUGB: need,
		VRAMTotalGB:  need,
		// ComfyUI's answer to insufficient VRAM is host RAM, so the two are
		// not independent: a box with too little turns offload into swap.
		RAMGB: int(sizing.DiffusionHostRAMBytes(k.bundle.TotalBytes) / sizing.GiB),
		// Disk holds the image, the bundle and the renders. Doubling the
		// bundle covers a partial file sitting beside its finished self
		// during a resumed fetch.
		DiskGB: int(k.bundle.TotalBytes/sizing.GiB)*2 + 40,
	}
	return claw.RaiseCriteria(base, want)
}

func (k *Kind) summary(plan core.SizingPlan) []string {
	out := []string{
		fmt.Sprintf("%s — %d nodes, %s serialisation",
			k.name, k.graph.Nodes, k.graph.Format),
	}
	if k.graph.Image.Width > 0 {
		line := fmt.Sprintf("render %dx%d", k.graph.Image.Width, k.graph.Image.Height)
		if k.graph.Image.Batch > 1 {
			line += fmt.Sprintf(" x%d", k.graph.Image.Batch)
		}
		if k.graph.Steps > 0 {
			line += fmt.Sprintf(", %d steps", k.graph.Steps)
		}
		out = append(out, line)
	}
	out = append(out, fmt.Sprintf("%d %s, %s to fetch",
		len(k.bundle.Items), plural(len(k.bundle.Items), "model"),
		sizing.HumanBytes(k.bundle.TotalBytes)))
	for _, it := range k.bundle.Items {
		out = append(out, fmt.Sprintf("  %-16s %s (%s)",
			it.Asset.Kind, it.Asset.Name, sizing.HumanBytes(it.Bytes)))
	}
	out = append(out, fmt.Sprintf("comfyui %s on %s", k.comfyRef(), k.image()))
	out = append(out, fmt.Sprintf("~%s VRAM (%s weights + %s working set)",
		sizing.HumanBytes(plan.RequiredVRAMBytes),
		sizing.HumanBytes(plan.WeightsBytes), sizing.HumanBytes(plan.KVCacheBytes)))
	return out
}

// caveats are the places this rig's guarantees are weaker than usual. Said out
// loud at bring-up, because a caveat nobody is shown is not a caveat.
func (k *Kind) caveats() []string {
	out := []string{
		"comfyui has no server-side credential; the loopback bind and the ssh tunnel are what protect it",
		"rendered images and any prompt text are visible to the host, which has root",
	}
	if k.graph != nil && !k.graph.Format.Executable() {
		out = append(out,
			"the graph is in the ui serialisation, which /prompt does not accept: "+
				"readiness will prove the server and the gpu, not a render")
	}
	if k.cfg.AllowPickle {
		out = append(out,
			"pickle containers were allowed: a .ckpt or .pth executes code on load, "+
				"on the host holding your hugging face token")
	}
	// Only when it actually differs. The example job names the default
	// explicitly, which is good practice and is not an override, and a caveat
	// that fires on every run is one nobody reads by the third.
	if k.image() != DefaultImage {
		out = append(out,
			"the image was overridden: the hardware floors were derived from a "+
				"different build, so re-derive them if this one differs")
	}
	return out
}

// Server builds the ComfyUI that runs on the rented box.
func (k *Kind) Server(*claw.Plan) runtime.Workload {
	r := New(k.graph, k.bundle, k.urls)
	r.AllowPickle = k.cfg.AllowPickle
	if k.cfg.Image != "" {
		r.ImageRef = k.cfg.Image
	}
	if k.cfg.ComfyUIVersion != "" {
		r.ComfyUIRef = k.cfg.ComfyUIVersion
	}
	return r
}

// Collect brings the renders back before the host is destroyed.
func (k *Kind) Collect(ctx context.Context, sess runtime.Session, dir string,
	since time.Time) (*claw.Result, error) {

	res, err := Sync(ctx, sess, dir, SyncOptions{Since: since})
	if err != nil {
		return nil, err
	}
	return &claw.Result{
		Saved: res.Saved, Skipped: res.Skipped, Failed: res.Failed,
		Bytes: res.Bytes, Dir: res.Dir,
	}, nil
}

// CountsAsWork reports which proxied requests are the operator using the rig.
//
// A browser changes the question entirely: ComfyUI's frontend reconnects a
// socket, re-polls its queue and refetches thumbnails for as long as a tab is
// open, so counting every request would mean the idle timeout never fires and
// the reclamation it implements is decorative.
func (k *Kind) CountsAsWork() func(*http.Request) bool { return CountsAsWork }

// HoldWhileBusy keeps the idle clock stopped while ComfyUI has work queued.
//
// A render is one short POST followed by minutes of work with no request
// outstanding at all, because the browser watches progress over a socket. To a
// timer measuring requests that is indistinguishable from an abandoned rig, and
// the default action on idle is destroy.
func (k *Kind) HoldWhileBusy(ctx context.Context, ep claw.LocalEndpoint, h claw.InFlight) {
	c := &Client{Addr: ep.Addr, Token: ep.Token, Probe: true}
	HoldWhileBusy(ctx, c, h, 15*time.Second)
}

func (k *Kind) image() string {
	if k.cfg.Image != "" {
		return k.cfg.Image
	}
	return DefaultImage
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// workflowName is what the rig is called in the journal and in status output.
// The file's own basename, because that is what the operator already calls it.
func workflowName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func (k *Kind) comfyRef() string {
	if k.cfg.ComfyUIVersion != "" {
		return k.cfg.ComfyUIVersion
	}
	return DefaultComfyUIRef
}
