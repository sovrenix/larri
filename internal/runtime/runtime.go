// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

// Package runtime is LARRI's second abstraction (P1).
//
// Nothing above this layer knows what is behind the endpoint. Implementations
// differ in exactly four places, and all four live inside one: how the payload
// is acquired, how VRAM fit is computed, what "ready" means, and what is
// enabled at launch.
//
// The abstraction has two tiers. Workload is the general one — anything LARRI
// rents hardware for and holds open — and Runtime is the inference engine
// case, which additionally promises the OpenAI-compatible /v1 surface of P2.
// vLLM, llama.cpp and Ollama are Runtimes; ComfyUI is a Workload and is not a
// Runtime, since it serves no /v1 at all.
package runtime

import (
	"context"
	"fmt"
	"io"
	"strings"

	"go.sovrenix.com/larri/internal/core"
	"go.sovrenix.com/larri/internal/secret"
)

// Endpoint is where a runtime serves, on the rented host.
//
// Host is always loopback. FR-SEC-08: the bind address is computed here and is
// not configurable — there is no flag, no config key, and a non-loopback value
// is rejected at launch rather than warned about. An inference server on a
// routable port is unauthenticated access to hardware the operator pays for,
// on a public IP shared with other tenants.
type Endpoint struct {
	Host  string
	Port  int
	Model string        // the stable served name, not the upstream ref
	Key   secret.Secret // the rig token; never a client's token (FR-SEC-22)

	// Probe marks this call as LARRI's own health check rather than the
	// operator's work, so it is excluded from the idle clock (FR-SUP-08).
	// A supervisor that reset the timer it enforces would never fire.
	Probe bool
}

// ProbeHeader marks a request as LARRI's own. It is duplicated from wire
// rather than imported to keep the runtime layer free of a dependency on the
// proxy; the constant is part of the contract between them, and the test in
// wire asserts they agree.
const ProbeHeader = "X-Larri-Probe"

// Loopback is the only address a runtime may bind.
const Loopback = "127.0.0.1"

// Valid reports whether the endpoint binds loopback as required.
func (e Endpoint) Valid() bool { return e.Host == Loopback || e.Host == "localhost" }

// Phase names a stage of bootstrap, for progress reporting.
type Phase string

const (
	PhaseImagePull       Phase = "image.pull"
	PhaseWeightsDownload Phase = "weights.download"
	PhaseLaunch          Phase = "launch"
	PhaseReady           Phase = "ready"
)

// Progress carries bootstrap progress so a multi-GB download does not look
// like a hang (FR-RT-06). Bytes are reported because the operator is paying
// for the time they take.
//
// Unsigned, like every other byte count here. They were int64, which put a
// conversion between a host's du output and the display, and a host is not
// trusted to keep what it prints inside an int64 (CodeQL go/incorrect-integer-
// conversion, alert 2).
type Progress struct {
	Phase       Phase
	Percent     float64
	BytesDone   uint64
	BytesTotal  uint64
	BytesPerSec float64
	Message     string
}

// Session is an authenticated channel to the rented host.
//
// It is satisfied by internal/sshx. Nothing here shells out: SSH is spoken
// in-process, so agent forwarding and friends are not implemented rather than
// disabled by a flag someone could re-enable in ~/.ssh/config (§15.5.2).
type Session interface {
	// Run executes a command and returns its combined output.
	Run(ctx context.Context, cmd string) ([]byte, error)
	// Dial opens a channel to a port on the host's loopback interface.
	Dial(ctx context.Context, port int) (io.ReadWriteCloser, error)
	// Close releases the session.
	Close() error
}

// Requirements are hardware constraints that must be checked BEFORE renting.
//
// They exist because the alternative is paying to discover them. A live run
// selected a GTX 1060 — the cheapest card whose VRAM held the model — and it
// could never have served with vLLM at any price, because Pascal is below the
// compute capability vLLM supports. VRAM fit answered the wrong question on
// its own.
type Requirements struct {
	// MinComputeCapability is the architecture level times 100: 700 for
	// Volta, 750 Turing, 800 Ampere, 890 Ada. Zero means no constraint.
	MinComputeCapability int

	// MinCUDA is the CUDA runtime the image is built against, times 10: 130
	// for 13.0. Vast reports the highest CUDA a host's driver supports as
	// cuda_max_good, and a host below the image's requirement cannot run it.
	// Zero means no constraint.
	MinCUDA int

	// TensorParallel says the engine spreads a model across cards by
	// splitting each layer's attention heads rather than by handing whole
	// layers to whole cards.
	//
	// It changes which multi-GPU hosts are worth renting. A layer split takes
	// any number of cards; tensor parallelism takes only degrees that divide
	// the head count, and vLLM discovers a degree that does not at engine
	// init — on a machine that is already billing. Selection therefore asks
	// sizing.Shards how many of a host's cards this engine can actually reach
	// and sizes against those, not against the number in the listing.
	TensorParallel bool

	// Vendor is the GPU vendor the image is built for, "" for any. Every
	// image LARRI runs today is CUDA, and an AMD card with 192 GB is the
	// cheapest large card RunPod lists at low stock — rented for a CUDA
	// image, it could never load a model.
	Vendor string

	// Why explains the constraint in the exclusion message, so an operator
	// seeing a cheap card rejected knows it was not arbitrary.
	Why string
}

// SatisfiesVendor reports whether an offer's GPU vendor can run the image.
// An offer that does not say passes, for the same reason an unreported
// compute capability does: failing closed on a missing field empties markets.
func (r Requirements) SatisfiesVendor(vendor string) (bool, string) {
	if r.Vendor == "" || vendor == "" || strings.EqualFold(r.Vendor, vendor) {
		return true, ""
	}
	return false, fmt.Sprintf("%s gpu, and the %s image is built for %s", vendor, r.Why, r.Vendor)
}

// SatisfiesCUDA reports whether a host's maximum usable CUDA version can run
// the image.
//
// Same failure as the capability floor and the same remedy: check before
// renting. Absence of data is again allowed through, because providers
// populate this field unevenly and failing closed would empty the market.
func (r Requirements) SatisfiesCUDA(hostCUDA float64) (bool, string) {
	if r.MinCUDA == 0 || hostCUDA <= 0 {
		return true, ""
	}
	if int(hostCUDA*10+0.5) >= r.MinCUDA {
		return true, ""
	}
	return false, fmt.Sprintf("cuda %.1f below the %.1f %s requires",
		hostCUDA, float64(r.MinCUDA)/10, r.Why)
}

// Satisfies reports whether hardware meets the requirement.
//
// An offer that does not report its capability is allowed through rather than
// excluded: absence of data is not evidence of incompatibility, and failing
// closed on a missing field would empty the market whenever a provider stopped
// populating it.
func (r Requirements) Satisfies(computeCapability int) (bool, string) {
	if r.MinComputeCapability == 0 || computeCapability == 0 {
		return true, ""
	}
	if computeCapability >= r.MinComputeCapability {
		return true, ""
	}
	return false, fmt.Sprintf("compute capability %.1f below the %.1f %s requires",
		float64(computeCapability)/100, float64(r.MinComputeCapability)/100, r.Why)
}

// WeightSizer is implemented by runtimes that know, before anything is
// rented, exactly how many bytes of weights they are going to load.
//
// Optional like the other capabilities: a runtime that cannot say simply does
// not implement it, and sizing falls back to estimating from the parameter
// count and the quantisation. The measurement is worth reaching for because
// that estimate is two approximations multiplied together, and a publisher
// who packs a quantisation differently than its name implies makes it wrong
// in the direction that OOMs.
//
// Zero means "I could not find out", never "no weights".
type WeightSizer interface {
	WeightBytes() uint64
}

// CredentialTaker is implemented by runtimes that need a credential to fetch
// weights.
//
// It is a separate interface rather than a field on ModelSpec because a token
// is a secret and ModelSpec is persisted in state (FR-STATE-05). Handing it
// over at launch keeps it out of every snapshot and journal entry that carries
// the spec.
type CredentialTaker interface {
	SetHuggingFaceToken(secret.Secret)
}

// Runtime is an inference engine: a Workload whose endpoint speaks the
// OpenAI-compatible /v1 surface of P2.
//
// The method set is Workload's exactly, and the type still exists because the
// distinction it names is real. "Runtime" is a promise about the wire format,
// and the wiring, the chat UI, and the IDE configuration all depend on that
// promise rather than on the lifecycle underneath it. An implementation must
// return ProtocolOpenAI from Protocol; TestRuntimesServeOpenAI checks that
// every compiled-in engine does, so the promise is enforced rather than
// documented.
type Runtime interface {
	Workload
}

// Adopter is implemented by runtimes that can re-attach to a server they
// already started, instead of starting a new one.
//
// This exists for recovery. When LARRI restarts it has lost the rig
// credential it minted at launch — held in memory only, so it never reached a
// snapshot — but the server on the host is still running, still holding the
// weights in VRAM. Relaunching to mint a fresh credential would evict them and
// pay for the load a second time, which is the expensive way to solve a
// bookkeeping problem.
//
// So Adopt recovers the credential from the running process rather than
// replacing it. That is only sound because the value was never a secret from
// the host to begin with: the host operator has root, and LARRI's threat model
// says so plainly (§15.4). It is a secret from the *network*, and adopting it
// over an authenticated channel keeps it one.
//
// Implementations must return a not-running error rather than a zero Endpoint
// when they find nothing, so a caller cannot mistake "no server" for "a server
// on port 0".
type Adopter interface {
	Adopt(ctx context.Context, sess Session, spec core.ModelSpec) (Endpoint, error)
}

// LivenessChecker is implemented by runtimes that can say whether their server
// process still exists on the host.
//
// This exists to stop LARRI paying for a decided outcome. Readiness waits are
// necessarily patient — a large model legitimately spends many minutes loading
// while writing nothing to its log — so the stall timeout that protects
// against a wedged host is long. A runtime that has *exited*, though, is not
// slow; it is finished, and every second spent waiting for it to speak is
// billed for an answer that already arrived.
//
// The distinction is only safe once the runtime has produced output. Before
// that, an absent process may simply not have started yet.
type LivenessChecker interface {
	Alive(ctx context.Context, sess Session) (bool, error)
}

// SecurityNoter is implemented by runtimes that cannot uphold some part of the
// security model, and must say so rather than let it pass unnoticed.
//
// The case that forced this: Ollama has no server-side credential at all. Its
// maintainers have said they do not plan to add one, so a rig running Ollama
// has no rig-side token, and the "two credentials with opposite lifetimes"
// boundary holds only on the local half. That is a defensible trade — the
// server binds loopback on the rented host, nothing publishes a port for it,
// and the SSH tunnel is the only way in — but it is not the same guarantee as
// vLLM's, and an operator choosing between them deserves to know which they are
// getting.
//
// Notes are surfaced at bring-up. A caveat nobody is shown is not a caveat.
type SecurityNoter interface {
	SecurityNotes() []string
}

// LogWriter is implemented by runtimes that redirect their server's output to
// a file on the host.
//
// The supervisor needs the *size* of that output, not its contents: growth is
// what distinguishes a model loading quietly from a host that has stopped
// trying, and it is what the two readiness regimes are built on (§12.2.1).
// Logs() returns contents and cannot answer that.
//
// This was a leak before it was an interface. readLogState reached for vLLM's
// constant directly, so under llama.cpp the daemon watched a file that never
// existed, concluded the runtime had produced nothing, and killed a host that
// was working perfectly.
type LogWriter interface {
	LogPath() string
}

// QuantAccepter is implemented by runtimes that can say which quantisation
// formats they serve.
//
// The formats are not interchangeable: vLLM reads safetensors-based schemes
// and cannot load a GGUF usefully, while llama.cpp and Ollama read GGUF and
// nothing else. Suggesting the wrong one wastes the operator's time in the
// same way the stale capability floor wasted their money.
type QuantAccepter interface {
	AcceptsQuant(quant string) bool
}

// ModelAdvisor is implemented by runtimes that can see a cheaper way to fetch
// the model the operator asked for.
//
// Distinct from refusing: the request is valid and will work. It is that the
// same repository often carries the same model at a quarter of the size, and
// the difference is paid in billed download time on every single rental. An
// operator who is told once can decide once; one who is never told pays it
// every run without knowing there was a choice.
//
// Advisory by construction — the notes are printed and nothing acts on them.
type ModelAdvisor interface {
	AdviseModel(ctx context.Context, spec core.ModelSpec) []string
}

// DefaultQuant is implemented by runtimes whose sensible default weight
// format differs from the generic one.
//
// llama.cpp and Ollama exist to run quantised weights on hardware that could
// not hold the full-precision model, so defaulting them to fp16 asks for the
// one format that defeats the point: on one repository it is 53.8 GB against
// 16.8 GB for Q4_K_M, a 22 GB card against a 128 GB box, and thirty-seven
// minutes of billed downloading against five.
type DefaultQuant interface {
	DefaultQuantization() string
}

// QuantizationFor is the weight format a runtime serves when nobody named one.
func QuantizationFor(r Runtime) string {
	if d, ok := r.(DefaultQuant); ok {
		return d.DefaultQuantization()
	}
	return "fp16"
}

// Weights is the file a runtime settled on, before anything is rented.
type Weights struct {
	File         string // the first part; what the engine is pointed at
	Bytes        uint64 // every part, summed; zero when the source publishes no sizes
	Quantization string // what the file carries, in the operator's spelling where they gave one
}

// WeightResolver is implemented by runtimes that must choose which of a
// repository's files to load, and that record the choice for Bootstrap.
//
// The daemon calls it while sizing, so every surface resolves the same way.
// It used to happen in the CLI, where it ran before the quantisation default
// was applied: `larri up` on a GGUF repository asked for no quantisation,
// took full precision — 15.3 GB where Q4_K_M is 5 — and the MCP server and
// the TUI, which resolved against an empty model, could not use llama.cpp at
// all. A default belongs to the engine, so the resolver applies its own
// rather than trusting a caller to have done it first.
type WeightResolver interface {
	ResolveWeights(ctx context.Context, spec core.ModelSpec) (Weights, error)
}

// HFEndpointSetter is implemented by runtimes that fetch weights from a
// Hugging Face-compatible host and can be pointed at a different one.
//
// It exists because a host that cannot reach huggingface.co is not
// necessarily a bad host: some regions cannot route to it at all, and the
// convention there is a mirror, which huggingface_hub honours through
// HF_ENDPOINT. Pointing at one is the operator's decision, not LARRI's —
// weights would then arrive from a third party — so nothing sets it by
// default.
type HFEndpointSetter interface {
	SetHuggingFaceEndpoint(endpoint string)
}

// WeightsProgressor is implemented by runtimes that can report how much of
// the model has landed on the host's disk.
//
// The rate alone does not answer the question an operator is actually asking.
// "net 14 MB/s" says something is moving; it does not say whether that is two
// minutes from finishing or forty, and the difference decides whether to wait
// or to destroy. Bytes on disk against bytes expected does answer it.
type WeightsProgressor interface {
	// WeightsOnDisk returns bytes fetched so far, or an error if it cannot
	// be measured. It is sampled during readiness and must be cheap.
	WeightsOnDisk(ctx context.Context, sess Session) (uint64, error)
}

// ToolCallingReporter is implemented by runtimes that can say, before a rig
// is used, whether tool calling will work for a given model.
//
// The failure it exists to prevent is silent and remote: vLLM serves happily
// without tool-call flags and answers ordinary chat, then refuses the first
// request carrying tools. The operator meets that inside a chat client, as
// `400 "auto" tool choice requires --enable-auto-tool-choice`, with nothing
// connecting it to the model they chose.
type ToolCallingReporter interface {
	ToolCallingNote(spec core.ModelSpec) string
}
