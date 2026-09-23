// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package whisper

import (
	"context"
	"fmt"
	"strings"

	"go.sovrenix.com/larri/internal/claw"
	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/sizing"
	"go.sovrenix.com/larri/internal/wire"
	"go.sovrenix.com/larri/internal/wire/clients"
)

// Type is the name the operator passes to --type.
const Type claw.Type = "whisper"

func init() {
	claw.Register(Type, func() claw.Kind { return &Kind{} })
}

// Config is the shape of a whisper job file.
type Config struct {
	// Model is a catalogue name or a full repository.
	Model string `yaml:"model"`

	// ComputeType is CTranslate2's precision.
	ComputeType string `yaml:"compute_type"`

	// Language pins the spoken language, skipping detection.
	Language string `yaml:"language"`

	// Concurrency is how many transcriptions may be in flight, which is what
	// the working set scales with.
	Concurrency int `yaml:"concurrency"`

	// Clients names the local applications to wire. Each becomes its own
	// credential, so one can be revoked without touching the others
	// (FR-SEC-23) and the probe can say which of them actually arrived.
	Clients []string `yaml:"clients"`

	// Image overrides the container.
	Image string `yaml:"image"`
}

// Kind is the speech-to-text claw.
type Kind struct {
	cfg  Config
	repo string

	// downloaded is what the host fetches; resident is what ends up in VRAM.
	// They differ whenever the operator asks for int8 (see ResidentBytes).
	downloaded uint64
	resident   uint64

	// measurer reads how large the repository is. Nil means Hugging Face.
	measurer Measurer
}

var (
	_ claw.Kind  = (*Kind)(nil)
	_ claw.Local = (*Kind)(nil)
)

func (k *Kind) Type() claw.Type { return Type }

// Site is local: the operator's own application stays on their machine and
// only the inference is rented, so there is nothing on the host to collect and
// a client configuration to put back.
func (k *Kind) Site() claw.Site { return claw.SiteLocal }

func (k *Kind) Describe() string {
	return "speech to text; wires a local transcription app"
}

// Plan reads the job file and says what to rent, without spending anything.
//
// One network call, to measure the repository. Everything else is arithmetic
// (§4a): a model name that does not exist, or a gated one the operator's token
// cannot read, ends the run here for nothing rather than on a rented host.
func (k *Kind) Plan(ctx context.Context, cfg *claw.Config, opt claw.Options) (*claw.Plan, error) {
	if err := cfg.Decode(&k.cfg); err != nil {
		return nil, err
	}
	if err := validClients(k.cfg.Clients); err != nil {
		return nil, err
	}
	if !ValidComputeType(k.computeType()) {
		return nil, errs.Newf(errs.ClassModelFailure, "whisper.Plan",
			"unknown compute type %q: expected one of %s",
			k.computeType(), strings.Join(ValidComputeTypes, ", "))
	}
	k.repo = Repo(k.cfg.Model)

	m := k.measurer
	if m == nil {
		m = NewHFMeasurer(opt.HFToken)
	}
	downloaded, err := m.Measure(ctx, k.repo)
	if err != nil {
		return nil, err
	}
	k.downloaded = downloaded
	k.resident = ResidentBytes(downloaded, k.computeType())

	plan, err := sizing.PlanSpeech(sizing.SpeechRequest{
		WeightBytes:     k.resident,
		FP16WeightBytes: downloaded,
		Concurrency:     k.cfg.Concurrency,
	})
	if err != nil {
		return nil, err
	}

	return &claw.Plan{
		Criteria: k.criteria(opt.Criteria, plan),
		Model: core.ModelSpec{
			Ref:        k.repo,
			Source:     core.SourceHuggingFace,
			ServedName: "whisper",
		},
		Sizing:         &plan,
		ColdStartBytes: downloaded,
		Summary:        k.summary(plan),
		Caveats:        k.caveats(),
	}, nil
}

// criteria turns the measurement into hardware, over whatever the operator
// asked for.
func (k *Kind) criteria(base core.Criteria, plan core.SizingPlan) core.Criteria {
	need := int((plan.RequiredVRAMBytes + sizing.GiB - 1) / sizing.GiB)
	want := core.Criteria{
		// Per-GPU and total are the same number: a transcription server pins
		// one device, so a requirement met only by summing cards is met
		// nowhere.
		VRAMPerGPUGB: need,
		VRAMTotalGB:  need,
		// Enough to load the model without swapping, with a floor for the
		// image and the operating system.
		RAMGB: ramFloorGB(k.downloaded),
		// The image, the download, and room for the cache to hold a partial
		// file beside its finished self during a resumed fetch.
		DiskGB: int(k.downloaded/sizing.GiB)*2 + 30,
	}
	return claw.RaiseCriteria(base, want)
}

func ramFloorGB(downloaded uint64) int {
	const floor = 8
	if n := int(downloaded/sizing.GiB) * 2; n > floor {
		return n
	}
	return floor
}

func (k *Kind) summary(plan core.SizingPlan) []string {
	out := []string{
		fmt.Sprintf("%s at %s", k.repo, k.computeType()),
		fmt.Sprintf("%s to fetch", sizing.HumanBytes(k.downloaded)),
	}
	if k.resident != k.downloaded {
		out = append(out, fmt.Sprintf(
			"quantised at load: %s downloaded, %s resident",
			sizing.HumanBytes(k.downloaded), sizing.HumanBytes(k.resident)))
	}
	if k.cfg.Language != "" {
		out = append(out, "language pinned to "+k.cfg.Language)
	}
	out = append(out, fmt.Sprintf("~%s VRAM (%s weights + %s working set)",
		sizing.HumanBytes(plan.RequiredVRAMBytes),
		sizing.HumanBytes(plan.WeightsBytes), sizing.HumanBytes(plan.KVCacheBytes)))
	out = append(out, "wiring "+joinOr(k.clientNames(), "nothing"))
	return out
}

// caveats are the places this rig's guarantees are weaker than usual.
func (k *Kind) caveats() []string {
	out := []string{
		"audio sent for transcription, and the text it produces, are visible to the host, which has root",
		"the transcription server has no credential of its own; the loopback bind and the ssh tunnel are what protect it",
	}
	// Said plainly rather than left for the operator to infer from a tier
	// letter: nothing on this machine is edited, so nothing has to be undone,
	// and the values have to be pasted by hand exactly once.
	out = append(out,
		"client configuration is guided, not written: larri prints the values and verifies by probe")
	if k.cfg.Image != "" && k.cfg.Image != DefaultImage {
		out = append(out,
			"the image was overridden: the hardware floors were derived from a "+
				"different build, so re-derive them if this one differs")
	}
	return out
}

// Server builds the transcription server that runs on the rented box.
func (k *Kind) Server(*claw.Plan) runtime.Workload {
	r := New()
	r.Model = k.repo
	r.ComputeType = k.computeType()
	r.Language = k.cfg.Language
	r.ModelBytes = k.downloaded
	if k.cfg.Image != "" {
		r.ImageRef = k.cfg.Image
	}
	return r
}

// Clients are the local applications to point at the endpoint.
//
// One writer per name, and the name is whatever the operator calls their
// application, because that is all this layer can honestly know: the writer is
// guided, so it configures nothing and needs to recognise nothing. What the
// name buys is real — its own credential, so one client can be revoked without
// rewiring the rest, and an identity the probe can attribute a request to.
func (k *Kind) Clients() []wire.ClientWriter {
	var out []wire.ClientWriter
	for _, name := range k.clientNames() {
		out = append(out, clients.NewOpenAIAudio(name))
	}
	return out
}

func (k *Kind) clientNames() []string {
	if len(k.cfg.Clients) == 0 {
		return []string{"transcriber"}
	}
	return k.cfg.Clients
}

func (k *Kind) computeType() string {
	if k.cfg.ComputeType == "" {
		return DefaultComputeType
	}
	return k.cfg.ComputeType
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	out := items[0]
	for _, s := range items[1:] {
		out += ", " + s
	}
	return out
}

// validClients refuses a client list that cannot give each client its own
// credential.
//
// A name is the whole identity here: the key is derived from it, the proxy
// records arrivals under it, and revocation is per name. Two entries spelled
// the same are therefore one client wearing two hats — same derived key, one
// surviving entry in the writer index, and a probe that cannot say which of
// them arrived. That is precisely the isolation FR-SEC-23 asks for, lost to a
// copy-paste in a config file.
//
// Refused in Plan, where it costs nothing, rather than discovered on a rig
// that is already billing (§4a).
func validClients(names []string) error {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			return errs.Newf(errs.ClassModelFailure, "whisper.Plan",
				"empty client name")
		}
		if seen[n] {
			return errs.Newf(errs.ClassModelFailure, "whisper.Plan",
				"duplicate client %q: each needs its own name to get its own key", n)
		}
		seen[n] = true
	}
	return nil
}
