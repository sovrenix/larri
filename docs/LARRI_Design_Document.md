# L.A.R.R.I.
# Design Document

**Local Agent for Remote Rigging of Inference**

---

| Field | Detail |
|---|---|
| **Document Title** | LARRI — Design Document |
| **Document ID** | LARRI-DES-001 |
| **Version** | 0.16 — Cheapest-With-Floors Selection |
| **Status** | Draft for Review |
| **Author** | Ram Katru |
| **Date** | 2026-08-21 |
| **Implements** | [LARRI Requirements Specification](LARRI_Requirements_Specification.md) (LARRI-REQ-001) |
| **Language** | Go |
| **Copyright** | © 2026 Sovrenix Inc. |
| **Licence** | GPL-3.0-or-later |

---

## Table of Contents

1. [Design Principles](#1-design-principles)
2. [System Architecture](#2-system-architecture)
3. [Package Layout](#3-package-layout)
4. [Core Domain Types](#4-core-domain-types)
5. [Provider Layer](#5-provider-layer)
6. [Runtime Layer](#6-runtime-layer)
7. [Sizing Engine](#7-sizing-engine)
8. [Selection Engine](#8-selection-engine)
9. [Provisioning Sequence](#9-provisioning-sequence)
10. [Wiring Layer](#10-wiring-layer)
11. [State Store and Reconciliation](#11-state-store-and-reconciliation)
12. [Supervisor](#12-supervisor)
13. [Teardown Protocol](#13-teardown-protocol)
14. [Daemon API and Surfaces](#14-daemon-api-and-surfaces)
15. [Configuration, Secrets, and Security](#15-configuration-secrets-and-security)
16. [Error Taxonomy](#16-error-taxonomy)
17. [Observability](#17-observability)
18. [Testing Strategy](#18-testing-strategy)
19. [Build and Release](#19-build-and-release)
20. [Implementation Milestones](#20-implementation-milestones)
21. [Appendix A — Worked Example](#appendix-a--worked-example)

---

## 1. Design Principles

Five rules. Every later section is an application of one of them.

| # | Principle | Consequence |
|---|---|---|
| **P1** | **Two abstractions, and only two.** | `Provider` and `Runtime`. Provider-specific vocabulary — Vast's offers/asks and bid pricing, RunPod's pods and Secure/Community Cloud — is normalized at the boundary. If core, ranking, wiring, or state code branches on which provider it is talking to, the boundary is wrong. Fix the boundary; do not add the branch. |
| **P2** | **OpenAI-compatible `/v1` is the internal contract.** | Nothing above the runtime layer knows whether llama.cpp, Ollama, or vLLM is behind the endpoint. Runtimes differ only in weight acquisition, VRAM fit math, and what "ready" means — all three live inside the `Runtime` implementation. |
| **P3** | **The local endpoint is stable; the remote host is not.** | Clients are configured once against a fixed loopback port. LARRI owns the hop from there to the current instance. The ephemeral provider host never reaches a client config file. |
| **P4** | **State is money.** | Intent is durably recorded before the call that spends. Destroy is idempotent and verified by re-query. Reconciliation runs on every daemon start. A crash may lose a process; it may never lose an instance. |
| **P5** | **Surfaces are clients, not owners.** | CLI, TUI, MCP, and web chat are four front-ends over one daemon API. A capability that works only from the CLI is in the wrong layer. |

---

## 2. System Architecture

```
 LOCAL MACHINE                                              RENTED HOST (provider)
┌────────────────────────────────────────────────┐         ┌──────────────────────────────┐
│                                                │         │                              │
│  ┌─────────┐ ┌─────┐ ┌─────┐ ┌──────────────┐  │         │   ┌──────────────────────┐   │
│  │   CLI   │ │ TUI │ │ MCP │ │ Web chat UI  │  │         │   │  Runtime process     │   │
│  └────┬────┘ └──┬──┘ └──┬──┘ └───┬──────────┘  │         │   │  vLLM / llama.cpp /  │   │
│       │         │       │        │             │         │   │  Ollama              │   │
│       └─────────┴───┬───┴────────┘             │         │   │  :8000  /v1/*        │   │
│                     │ HTTP over unix socket    │         │   └──────────┬───────────┘   │
│              ┌──────▼───────────────────┐      │         │              │               │
│              │        DAEMON            │      │         │   ┌──────────┴───────────┐   │
│              │  ┌────────────────────┐  │      │         │   │  sshd                │   │
│              │  │ Orchestrator       │  │      │         │   └──────────┬───────────┘   │
│              │  │ Supervisor         │  │      │         └──────────────┼───────────────┘
│              │  │ Cost accountant    │  │      │                        │
│              │  └────────────────────┘  │      │                        │
│              │  state │ rank │ sizing   │      │                        │
│              │  provider │ runtime │wire│      │                        │
│              └──────┬─────────────┬─────┘      │                        │
│                     │             │            │                        │
│         ┌───────────▼──┐   ┌──────▼──────────┐ │                        │
│         │ state store  │   │ tunnel manager  │─┼── ssh -L ──────────────┘
│         │ ~/.local/    │   │ 127.0.0.1:8000  │ │
│         │ state/larri  │   └──────▲──────────┘ │        ┌─────────────────────────┐
│         └──────────────┘          │            │        │  Provider control API   │
│                                   │            │◄──────►│  Vast.ai / RunPod       │
│   IDE + chat clients ─────────────┘            │  HTTPS └─────────────────────────┘
│   (configured once, never rewired)             │
└────────────────────────────────────────────────┘
```

Three planes, deliberately separated:

- **Control plane** — daemon ↔ provider HTTPS API. Search, create, status, destroy. Low
  volume, high consequence, every call reconcilable.
- **Data plane** — client → `127.0.0.1:8000/v1` → tunnel → remote runtime. High volume,
  low consequence, and completely unaware of the control plane. A data-plane request must
  never require a control-plane call.
- **Telemetry plane** — daemon ← rig samples, daemon → operator surfaces, and optionally
  daemon → an external collector. Spends nothing, serves nothing, and is subordinate to
  both planes above: it may never affect either (§17.1).

The daemon is the only component that mutates state. Surfaces read and issue commands.

---

## 3. Package Layout

```
cmd/larri/              CLI entrypoint; subcommands up, down, status, offers,
                        logs, orphans, daemon, mcp, ui
internal/core/          Normalised domain vocabulary (§4) — shared by provider,
                        sizing, rank, state, runtime, daemon
internal/secret/        The Secret type (§15.2)
internal/errs/          The error taxonomy (§16)
internal/provider/      Provider interface, registry, normalization
    vastai/             Vast.ai adapter
    runpod/             RunPod adapter
internal/runtime/       Runtime interface, selection heuristic
    llamacpp/  ollama/  vllm/
internal/sizing/        VRAM / KV-cache / context math; model fact catalogue
internal/rank/          Offer scoring
internal/state/         Durable store, journal, reconciliation
internal/wire/          Tunnel manager, local proxy, client config writers
    clients/            Per-client config adapters (IDE, chat)
internal/daemon/        Orchestrator, supervisor, cost accountant, HTTP API
internal/tools/         Canonical LARRI tool registry — one definition per tool
internal/mcpsrv/        MCP adapter over internal/tools (drives Claude Code et al.)
internal/telemetry/     OTel setup, exporters, metric collectors, ring buffer
    hostmet/            Rented-host GPU/CPU/RAM sampling over SSH
    runtimemet/         Prometheus scrape of the runtime's own /metrics
internal/tui/           Bubble Tea dashboard
internal/webui/         Embedded chat pane + KPI console (go:embed assets)
internal/sshx/          In-process SSH (x/crypto/ssh): dial, host-key pinning,
                        port-forward, exec channels, connection reuse
internal/config/        Config file, profiles, secret resolution
docs/                   This document and LARRI-REQ-001
```

Dependency rule: `provider`, `runtime`, `sizing`, `rank`, `state`, `wire` know nothing
about `daemon`. `daemon` composes them. Surfaces depend only on the daemon API client.
`telemetry` is depended upon by anything that emits, and depends on nothing but the OTel
API — so a package can be instrumented without acquiring a dependency on the daemon.

---

## 4. Core Domain Types

The normalized vocabulary. These types are the contract between layers; everything
provider-specific dies at the adapter boundary (P1).

```go
// Criteria is what the operator asks for.
type Criteria struct {
    GPUModel      []string      // ["A100", "H100"] — empty means any
    GPUCount      int
    VRAMPerGPUGB  int
    VRAMTotalGB   int
    CPUCores      int
    RAMGB         int
    DiskGB        int
    Regions       []string      // allow-list; empty means any
    BlockRegions  []string
    MaxPriceHr    float64       // USD
    Interruptible Tristate      // require / forbid / allow
    MinReliability float64      // 0..1, provider-reported
    Providers     []string      // restrict search; empty means all enabled
    CertifiedOnly bool          // datacentre-certified hosts only (§15.5.1)
}

// ModelSpec is what the operator wants served.
type ModelSpec struct {
    Ref          string   // "Qwen/Qwen3-Coder-30B", "llama3.1:70b", or a local path
    Source       Source   // HuggingFace | OllamaRegistry | LocalPath | URL
    Quantization string   // "fp16", "q4_K_M", "awq", "gptq-int4"
    ContextLen   int
    ServedName   string   // stable wire-format name, independent of Ref
    Gated        bool     // requires a token to download
    ToolCalling  Tristate // require / forbid / allow — see §6.6
    ToolParser   string   // runtime-specific parser id; empty means auto-detect
}

// Offer is a normalized purchasable unit.
type Offer struct {
    Provider      string
    OfferID       string
    GPUModel      string
    GPUCount      int
    VRAMPerGPUGB  int
    CPUCores      int
    RAMGB         int
    DiskGB        int
    Region        string
    PriceHr       float64
    Interruptible bool
    Reliability   float64
    NetDownMbps   float64   // matters: weight download time is billed time
    CUDAVersion   string    // max CUDA the host driver supports — image selection (§6.5)
    DriverVersion string    // NVIDIA driver, where the provider reports it
    Raw           json.RawMessage // provider payload, for debugging only
}

// Instance is a live provider resource.
type Instance struct {
    Provider   string
    InstanceID string
    OfferID    string
    PriceHr    float64
    SSHHost    string
    SSHPort    int
    PublicIP   string
    PortMap    map[int]int // container port -> external port
    CreatedAt  time.Time
    Labels     map[string]string // includes the LARRI ownership marker
}

// Rig is the whole managed unit and the root of persisted state.
type Rig struct {
    ID        string        // ULID, assigned before any provider call
    State     LifecycleState
    Criteria  Criteria
    Model     ModelSpec
    Runtime   RuntimeKind
    Offer     Offer
    Instance  *Instance     // nil until CREATING resolves
    Plan      SizingPlan
    LocalPort int
    Wiring    []WiringRecord // what was changed locally, for revert
    History   []Transition
    CreatedAt time.Time
    End       *Termination  // nil while the rig lives; why it died once it does
}

// Termination is the answer to "why is my rig gone", recorded at teardown and
// retained after it. Typed rather than a free-text note, for the same reason the
// error taxonomy (§16) is a type: the value drives display and policy in several
// places, and a string would drift per call site and be unqueryable afterwards.
type Termination struct {
    Actor    Actor             // who decided — see below
    Code     ReasonCode        // OperatorRequest, IdleTimeout, BudgetCeiling,
                               // Preempted, HostFailure, ProvisionDeadline,
                               // BootstrapFailed, OrphanSweep, PanicSweep
    At       time.Time
    Summary  string            // one evidence-bearing line, shown in status
    Evidence map[string]string // the structured facts behind Summary
    Cost     CostSummary       // total spent, and what phase it went to
}
```

`Rig.ID` is minted **before** the first provider call and stamped onto the provider
resource as a label (FR-STATE-04). That label is what makes orphan attribution possible.

---

## 5. Provider Layer

### 5.1 Interface

Deliberately narrow — narrow enough to fake in tests (NFR-09), which is the real design
constraint.

```go
type Provider interface {
    Name() string
    Search(ctx context.Context, c Criteria) ([]Offer, error)
    Create(ctx context.Context, o Offer, spec CreateSpec) (*Instance, error)
    Get(ctx context.Context, instanceID string) (*Instance, error)
    List(ctx context.Context) ([]Instance, error)   // ALL instances — orphan detection
    Destroy(ctx context.Context, instanceID string) error
}

type CreateSpec struct {
    Image        string
    SSHPublicKey string
    DiskGB       int
    Env          map[string]string
    Ports        []int
    Label        string  // "larri:<rigID>"
    OnStart      string  // bootstrap command, if the provider supports one
}
```

`List` returning **all** instances, not just LARRI's, is intentional: the reconciler needs
to see everything to distinguish LARRI orphans from the operator's own instances (P4).

### 5.2 Adapter Notes

| Concern | Vast.ai | RunPod |
|---|---|---|
| Unit of capacity | Marketplace *offer* (a specific machine), on-demand or interruptible bid | *Pod* on a GPU type, in Secure Cloud or Community Cloud |
| Selection | Search offers with filters, pick a machine | Pick a GPU type + cloud tier; the platform places it |
| Price model | Per-offer $/hr, bid price for interruptible | Per-GPU-type $/hr, tier-dependent; spot available |
| Access | SSH host + mapped port; direct port mapping available | SSH; HTTP proxy for exposed ports |
| Reliability signal | Provider-reported host reliability score | Tier (Secure > Community) |

Normalization rules:

- **Reliability** → 0..1 in `Offer.Reliability`. Where a provider exposes only a tier,
  map the tier to a fixed value and document the mapping in the adapter.
- **Interruptible** → a single bool. Bid mechanics stay inside the adapter; core code
  never sees a bid price separate from `PriceHr`.
- **Placement** → for providers that place rather than let you pick, `OfferID` identifies
  the *class* requested and `Instance` carries what was actually placed. Ranking operates
  on the class; the sizing check re-runs against the placed instance before bootstrap.

### 5.3 The Ownership Marker Is Provider-Neutral

Every provider gets one string of LARRI's choosing stamped on the resource, and
each platform calls it something different: Vast has `label`, RunPod names pods,
and a future adapter will have its own field with its own length cap. The
*content* is therefore defined once in `core` and the adapter only decides where
to put it and how much of it fits.

What the marker carries, and why more than a rig ID:

| | |
|---|---|
| Rig ID | Attribution. The one field that must survive anything (FR-STATE-04) |
| Model, served name, runtime | What an orphan was doing, so a report explains rather than merely alerts |
| Created-at, price | Whether it is worth destroying immediately and what it has cost since |
| Local port | Which client configuration points at it |

It exists for the case where **local state is gone entirely** — a lost disk, a
new laptop, a corrupted directory — and the only surviving record of a billing
resource is what was stamped on it. A bare rig ID is a puzzle; "rig 01J9Z
serving Qwen3-Coder on vllm since Tuesday at $1.29/hr" is a decision.

Three constraints shape the encoding and they pull against each other:

- **The host reads it.** The detail fields are sealed with AES-256-GCM under a
  key the operator supplies (§15.5.4). Model and runtime are visible in the
  host's own process table anyway, but the marker should not volunteer them,
  and the local port says something about the operator's machine rather than
  the rented one.
- **The rig ID is deliberately *not* sealed.** Attribution has to survive losing
  the key. An operator who reinstalls without their key still needs
  `larri orphans` to say "this is yours"; a fully encrypted marker would turn
  every surviving rig into an unattributable stranger's instance at exactly the
  moment that costs money.
- **Truncation degrades rather than destroys.** Providers cap marker length
  without always documenting where, so the rig ID comes first and detail is
  appended in decreasing order of usefulness. Unknown keys are ignored, so a
  marker written by a later LARRI stays attributable to an earlier one.

Adapters map it as follows, and a new provider answers the same two questions:
which field survives a restart and is readable back, and how long may it be.

| Provider | Field | Cap |
|---|---|---|
| Vast.ai | `label` on the instance | conservative default until measured |
| RunPod | pod name / metadata | to be confirmed when the adapter lands (M3) |

**Decoding discipline (R-02).** On a shape LARRI cannot parse, fail loudly: a mis-parsed
price or VRAM figure is worse than an outright error, because it spends money on a wrong
assumption.

The obvious implementation is wrong, and the Vast adapter is where that became clear.
Rejecting unknown fields sounds like the strict option; against an API whose offer payload
carries over a hundred fields when LARRI models about fifteen, it flags every response and
the signal is noise within a week. **Adding** a field is routine and harmless. The hazard is
a field LARRI *depends on* being renamed, removed, or changed in unit.

So the strictness is targeted: spend-relevant fields decode as pointers, so absence is
distinguishable from zero, and normalisation range-checks them against physical plausibility.
Vast's `gpu_ram` arrives in MiB — reading 81920 as gigabytes would present an A100 as an
80 TB card, pass every sizing check, and OOM after the operator paid to boot it. A live run
across 500 offers normalised with no violations, which is what makes the unit assumption
verified rather than assumed.

**Result sets are capped, and the cap does not announce itself.** Vast's search returns at
most the requested limit with no truncation flag, sorted by price ascending. A full page is
therefore the only available signal that more matched, and the offers dropped are the more
expensive, better-fitting cards a value-weighted ranking most needs to weigh (§8). Adapters
must report a full page as truncated rather than let a round number pass for a complete
picture.

**API versions differ per endpoint.** On Vast, search and create are `v0` while the instance
listing is `v1`, and the listing is capped at 25 per page with keyset pagination — a change
the provider itself flags as breaking. An adapter that assumed one version, or read one page,
would miss every orphan past the twenty-fifth: R-01 arriving through a default rather than a
mistake. `List` pages to exhaustion, applies no status filter, and each endpoint's version is
stated separately.

---

## 6. Runtime Layer

### 6.1 Interface

```go
type Runtime interface {
    Kind() RuntimeKind
    Image(spec ModelSpec, plan SizingPlan) string
    Bootstrap(ctx context.Context, sess Session, spec ModelSpec, plan SizingPlan, progress chan<- Progress) error
    Launch(ctx context.Context, sess Session, spec ModelSpec, plan SizingPlan) (Endpoint, error)
    Ready(ctx context.Context, ep Endpoint, spec ModelSpec) error  // real completion
    Logs(ctx context.Context, sess Session, tail int) (io.ReadCloser, error)
    Stop(ctx context.Context, sess Session) error
}
```

`Session` is an SSH exec session against the instance. `Progress` carries phase, percent,
and bytes so a 40-GB weight download does not look like a hang (FR-RT-06).

### 6.2 Per-Runtime Differences

| | llama.cpp | Ollama | vLLM |
|---|---|---|---|
| Weight format | GGUF | Ollama registry blobs | safetensors |
| Acquisition | HF download of a single GGUF file | `ollama pull <tag>` | HF snapshot download of the repo |
| Fit strategy | Layer offload — can spill to CPU, so it survives under-provisioned VRAM at a throughput cost | Same engine underneath, managed | Must fit in VRAM; `--gpu-memory-utilization` fraction |
| Key launch flags | `-m <gguf> -c <ctx> -ngl <layers> --host 127.0.0.1 --port 8000` | `OLLAMA_HOST=127.0.0.1:8000` + pull | `vllm serve <ref> --host 127.0.0.1 --port 8000 --served-model-name <n> --max-model-len <ctx> --gpu-memory-utilization <f> --tensor-parallel-size <n> --api-key <key>` |
| Bind address | **`127.0.0.1` on the remote host — never a routable interface**, and not configurable (§15.5) | same | same |
| Multi-GPU | Limited | Limited | Tensor parallel across N GPUs |
| Tool calling | `--jinja` with the model's chat template; parser inferred from it | Template-driven; support varies by tag | `--enable-auto-tool-choice --tool-call-parser <id>`, where `<id>` is model-family-specific (`hermes`, `llama3_json`, `mistral`, …) |
| Ready signal | `/v1/chat/completions` round-trip | same | same |

### 6.3 Selection Heuristic (FR-RT-02)

```
if spec.Quantization is a GGUF quant  or  spec.Ref ends in .gguf   → llamacpp
else if spec.Source == OllamaRegistry                              → ollama
else if plan.FitsInVRAM and GPUCount >= 1                          → vllm
else                                                               → llamacpp (offload)
```

Always overridable with `--runtime`. The heuristic's job is to make the common case
flagless, not to be clever.

### 6.4 Served-Model Name (FR-RT-04)

The runtime is always launched with an explicit stable served name. Clients are configured
against that name. Changing the upstream model repo, quantization, or even the runtime must
not require touching client config — this is P3 applied to the model identifier rather than
the host.

### 6.5 Images Are Pre-Baked and Pinned (FR-RT-11, Q-07)

LARRI publishes and maintains a runtime image per `RuntimeKind` rather than composing stock
images on the fly (Q-07, resolved). The argument is bootstrap determinism: a stock image
plus a `pip install` at boot is a fresh dependency resolution on every rig, on someone
else's hardware, on a billing clock — and R-03 says that clock is the expensive part.

A pre-baked image carries the runtime, the CUDA userspace it was built against, `nvidia-smi`
and the host-metrics tooling §17.4 depends on, and the readiness probe. What it does *not*
carry is weights, which stay the one genuinely large download.

Four rules make this safe rather than merely convenient:

- **Pinned by digest, never by tag.** `CreateSpec.Image` resolves to
  `ghcr.io/…/larri-vllm@sha256:…`. A moving tag means two rigs created a minute apart can run
  different code, which makes a bootstrap failure unreproducible.
- **Selected against the host's driver.** A CUDA userspace newer than the host driver fails
  at launch, after the instance is paid for. This is why `Offer` carries `CUDAVersion` — image
  variant selection is a *search filter*, not a post-create discovery. An offer whose driver
  cannot run any published variant is filtered out before ranking.
- **Stock-image fallback stays supported.** New GPUs and unusual driver versions appear
  faster than an image matrix can track them. Falling back is a warning and a slower boot,
  not a failure — and the fallback path is exercised in tests so it does not rot.
- **Pre-baking is not attestation.** The host still controls what actually runs (FR-SEC-05).
  The digest proves what LARRI *asked* for, and nothing about what executed. Pre-baked images
  buy reproducibility and speed; they buy no trust.

The maintenance burden is real and is accepted deliberately: a CUDA/driver matrix, a runtime
version to track, and a publishing cadence. It is the price of `READY` meaning the same thing
on every rig.

### 6.5.1 The Instance *Is* the Container

A marketplace instance is not a machine you install software onto. The image
named in `CreateSpec` is what the instance **runs**: the provider creates a
container from it, the start-up script executes inside it, and SSH connects
into it.

This is easy to get backwards, and getting it backwards is expensive. An
earlier implementation treated the instance as a VM — `docker pull` in
bootstrap, `docker run` in launch — which is docker-in-docker inside a
container with no daemon. Every live bring-up died with

```
bash: line 1: docker: command not found
```

*after* successfully probing the endpoint, pinning the host key and verifying
the GPU, so the failure arrived at the one step that looked like infrastructure
and read like a network problem.

Three consequences that generalise beyond Vast, and that a new adapter should
answer before it is written:

- **The runtime arrives with the instance.** Bootstrap's job is to *verify*
  what turned up, not to fetch it. The image is chosen at create time and is
  therefore a search input (§6.5), not a bootstrap step.
- **The launcher is discovered, not assumed.** Images package the same runtime
  as a console script or a module entrypoint. Guessing one shape is how a
  bring-up fails on a host that was fine, so LARRI asks the host how to start
  what it has.
- **A missing runtime is a *model* failure, not a host failure.** The next
  machine runs the same image and fails identically, so falling back only
  spends more (FR-PROV-05). The error taxonomy is what stops the retry, which
  is why that classification is a type rather than a convention (§16).

The server is started detached, with its output captured to a file. A process
that dies with the SSH exec channel that launched it leaves readiness chasing
something that was never going to be there, and a launch that fails before the
server answers leaves the log as the only account of why.

### 6.6 Tool Calling Is a Launch-Time Property (FR-RT-09)

The chat pane drives LARRI by having the served model emit tool calls (§14.4.4), which makes
tool-calling support a property of the *launch*, not of the request. A runtime started
without it will accept a request carrying `tools[]` and simply answer in prose — the failure
is silent, and it looks like a bad model rather than a missing flag.

This is the fourth per-runtime difference, alongside the three in P2, and it belongs inside
the `Runtime` implementation for the same reason the others do: vLLM needs
`--enable-auto-tool-choice` plus a `--tool-call-parser` whose value depends on the model
family; llama.cpp infers the parser from the chat template under `--jinja`; Ollama's support
varies by tag. `ModelSpec.ToolParser` exists as an override for when the mapping from model
family to parser is not in the bundled catalogue.

Two consequences the operator sees:

- **Capability is not uniform.** Small quantized models emit malformed tool calls often
  enough that the pane must treat a parse failure as an ordinary outcome — render what the
  model said and move on — rather than as an error worth surfacing as a defect.
- **`ToolCalling: require` is a pre-spend check.** If the operator asks for a control-capable
  rig and neither the catalogue nor the model's chat template indicates tool-calling support,
  that is `ErrModelFailure` before the create call, not a discovery made after paying to
  boot.

---

## 7. Sizing Engine

Single package, single source of truth, consumed in three places (search filter, ranking,
launch flags). The invariant that makes over-commitment impossible to introduce in only one
of the three.

### 7.1 Model Facts

```go
type ModelFacts struct {
    Params        float64 // billions
    Layers        int
    KVHeads       int     // GQA: often << attention heads
    HeadDim       int
    HiddenSize    int
    MaxContextLen int
}
```

Resolved by **live fetch from Hugging Face** (Q-06, resolved): the model's `config.json` is
the authority, because it is the same file the runtime will read, and a bundled catalogue
starts drifting the day it ships. Resolution order:

1. **Operator override** — always wins, for models whose config is wrong or absent.
2. **Local cache** — `~/.cache/larri/modelfacts/<ref>@<revision>.json`, keyed by the resolved
   commit rather than the branch name, so a cache hit is a fact about an immutable revision
   and never goes stale.
3. **Live fetch** from Hugging Face, then cached.
4. **Hard error.** Unresolvable facts are never guessed — a fabricated layer count produces a
   confident VRAM figure that is wrong, which is worse than refusing (P4, NFR-11).

Three consequences of choosing live over bundled:

- **Gated models need `HF_TOKEN` at plan time, and that is a feature.** Fetching `config.json`
  for a gated repo exercises the same credential the weight download will need. A token that
  cannot read the repo now fails during sizing, before a single second is billed, instead of
  forty minutes into a bootstrap on a rented A100.
- **Quantized GGUF repos frequently have no usable `config.json`.** The fallback is the GGUF
  file's own metadata header, and failing that, the base model the quantization was derived
  from — recorded explicitly, because inferring the base model from a repo name is guessing
  by another route.
- **Sizing acquires a network dependency.** The cache is what keeps it from being a hard one,
  and cache-only operation is a supported mode rather than a degraded accident.

### 7.2 The Math

```
bytesPerWeight  = quantBits / 8                    // fp16 → 2, q4_K_M → ~0.5625
weightsBytes    = Params × 1e9 × bytesPerWeight

kvBytes         = 2 × Layers × KVHeads × HeadDim × ContextLen × concurrency × kvElemBytes
                  // the leading 2 is K and V

activationBytes ≈ hiddenSize × ContextLen × batch × elemBytes × activationFactor

overhead        = CUDA context + fragmentation + runtime allocator
                  ≈ max(1.0 GiB, 0.08 × (weightsBytes + kvBytes))

requiredVRAM    = (weightsBytes + kvBytes + activationBytes + overhead) × safetyFactor
```

`safetyFactor` defaults to 1.10. `concurrency` defaults to 1 and is configurable — the KV
cache scales linearly with it, and it is the single most common cause of an OOM that only
appears under load rather than at boot.

### 7.3 Output

```go
type SizingPlan struct {
    RequiredVRAMBytes  uint64
    WeightsBytes       uint64
    KVCacheBytes       uint64
    FitsInVRAM         bool
    TensorParallelSize int
    GPUMemUtilization  float64  // vLLM
    OffloadLayers      int      // llama.cpp: -ngl
    ContextLen         int      // possibly reduced from requested
    Warnings           []string
}
```

When the requested context does not fit, the planner reduces `ContextLen` to what fits and
records a warning — it does not silently accept the requested value. When even the weights
do not fit, `FitsInVRAM` is false, and for vLLM that is a pre-spend rejection with the
shortfall named (FR-CRIT-06, NFR-11):

```
✗ Qwen3-Coder-30B @ fp16, 32k context needs ~68 GB VRAM.
  Best matching offer: RTX 4090 24GB ($0.34/hr) — 44 GB short.
  Cheapest offer that would fit: A100 80GB ($1.29/hr).
  Try: --quantization q4_K_M (~19 GB) or --context 8192 (~62 GB).
```

---

## 8. Selection Engine

**Criteria are a hard filter. Among everything that survives it, LARRI takes the cheapest
offer that passes the safety floors.**

That is the product goal stated plainly: the operator says what they need, and LARRI finds
the least expensive hardware that provides it. Anything more elaborate has to justify
charging them more than they asked to pay.

An earlier revision of this section specified a weighted score across price, fit,
reliability, bandwidth, and region, and asserted that "ranking optimises for value, not raw
price". Two things were wrong with it. Appendix A's worked example — a $1.29 A100 beating a
$0.81 A6000 — **cannot be produced by the formula it illustrates**: scored with those
weights the A6000 reaches 0.892 against the A100's 0.711, and the A100's claimed 0.81 is
above its arithmetic ceiling of 0.717. And the example's premise was confused, because there
the cheaper card was also the better-fitting one, so price and fit agreed rather than traded
off. A worked example that cannot occur is worse than none: it would have taught whoever
implemented this the opposite of what the formula does.

### 8.1 The Filters

Each is a hard gate, and each records **why** it excluded an offer. FR-SRCH-03 requires
selection to be inspectable, and an operator asking "why not the cheap one" deserves the
actual reason rather than a score.

| Gate | Rejects | Rationale |
|---|---|---|
| **Criteria** | GPU model, count, region, disk, max price | What the operator asked for |
| **Fit** | Offers whose VRAM cannot hold the model | §7's plan; a rig that OOMs is not cheap |
| **Reliability floor** | Below `reliability_floor`, default 0.90 | A host that vanishes mid-run costs more than it saved |
| **Price outlier** | Anomalously below the class median | §8.2 |
| **Interruptible** | Bid offers unless required (Q-04) | A preempted instance still bills storage |

Fit is a *filter* here rather than a scoring term. It answers one question — will the model
run — and once answered it has no further business competing with price. An 80 GB card
serving a 19 GB model is wasteful, but it is not wrong, and if it is the cheapest thing that
works then it is the right answer.

### 8.2 Anomalously Cheap Is a Signal, Not a Bargain (FR-SRCH-08)

Ranking by price walks straight toward whatever a host fishing for prompts and tokens would
list, so the floor is not optional. Live Vast data for RTX 4090: n=208, min $0.135, mean
$0.951, max $8.002.

The mean is useless here — dragged upward by a tail of listings at $8/hr, which is H100
territory and not the market. So the test is against the **median for the GPU model class**,
with spread measured by median absolute deviation, and it needs a **minimum sample** before
it judges anything: a model with four listings has no distribution to be an outlier in.

An offer far below its class median is excluded and *reported*, not silently dropped. It may
be a genuine bargain on tired hardware, and the operator can lower the floor deliberately —
but they should do it knowing what they are overriding.

### 8.3 What the Operator Sees

Selection prints the choice **and the road not taken**, so the cost of the safety floors is
visible rather than implicit:

```console
  ranked    1. vastai  RTX 4090 24GB  $0.42/hr  rel 0.97   ← selected
            excluded:
              RTX 4090 24GB  $0.135/hr  reliability 0.61 below floor 0.90
              RTX 4090 24GB  $0.19/hr   4.9× below class median $0.94
              A100 80GB      $1.29/hr   costlier than the selection
```

An operator who wants the $0.135 machine can have it. What they cannot do is get it by
accident.

Ties are broken deterministically — by reliability, then by offer ID — so the same market
produces the same choice twice, which is what makes a selection reproducible in a bug report.

## 9. Provisioning Sequence

```
Operator                Daemon              State           Provider          Host
   │                      │                   │                │               │
   │ up --model … ───────►│                   │                │               │
   │                      │ resolve facts     │                │               │
   │                      │ sizing plan       │                │               │
   │                      │◄── reject if no fit (no spend) ────┤               │
   │                      │ Search ──────────────────────────► │               │
   │                      │◄──── offers ───────────────────────┤               │
   │                      │ rank + select     │                │               │
   │◄─ confirm (interactive) ─────────────────┤                │               │
   │                      │ mint RigID        │                │               │
   │                      │ WRITE INTENT ────►│  ①            │               │
   │                      │ Create ──────────────────────────► │               │
   │                      │◄──── instance ─────────────────────┤               │
   │                      │ persist instance ►│  ②            │               │
   │                      │ wait for sshd ────────────────────────────────────►│
   │                      │ re-verify sizing vs placed hardware│               │
   │                      │ Bootstrap (image, weights) ───────────────────────►│
   │◄─ progress stream ───┤                   │                │               │
   │                      │ Launch runtime ───────────────────────────────────►│
   │                      │ open tunnel :8000 ────────────────────────────────►│
   │                      │ Ready(): real completion ─────────────────────────►│
   │                      │◄──────────── completion ──────────────────────────┤│
   │                      │ wire clients      │  ③            │               │
   │                      │ state = READY ───►│                │               │
   │◄─ endpoint ready ────┤                   │                │               │
```

Three durability points:

- **①** — the intent write. Precedes the spend. If the process dies anywhere after this
  line, reconciliation can find what was created (P4, FR-PROV-01).
- **②** — the instance record. Makes destroy possible without a provider search.
- **③** — the wiring record. Makes revert possible on `down` (FR-WIRE-05).

**Create-call failure handling (FR-PROV-03).** A timeout or transport error is *not* a
failure — it is an unknown. The handler calls `List`, looks for the rig label, and adopts
the instance if it exists. Blind retry is the one thing that must never happen here (R-07).

**Deadline (FR-PROV-04).** The whole sequence runs under a single deadline. On expiry the
orchestrator transitions to `DRAINING` and runs teardown; a half-provisioned rig is
destroyed, never abandoned.

**Fallback (FR-PROV-05).** Host-attributable failures — sshd never comes up, GPU not
visible, image pull fails — retry against the next-ranked offer, bounded. Model- or
config-attributable failures do not retry, because the next host will fail identically.

---

## 10. Wiring Layer

The layer that makes P3 real.

### 10.1 Tunnel Manager

Owns the fixed local port for the rig's lifetime. The transport is an SSH local
port-forward, spoken **in-process** with `golang.org/x/crypto/ssh` rather than by shelling
out to an `ssh` binary (§15.5.2). Conceptually the equivalent of:

```
ssh -N -L 127.0.0.1:<localPort>:127.0.0.1:<remotePort> -p <sshPort> root@<sshHost>
```

but as owned code rather than a supervised child process:

```go
net.Listen("tcp", "127.0.0.1:<localPort>")   // bind first - failure is a Go error
ssh.Dial(...)                                // HostKeyCallback = the pinned key
client.Dial("tcp", "127.0.0.1:<remotePort>") // a channel we hold
io.Copy both ways
```

The listener binds **before** the rig is declared healthy, which is the structural version
of `ExitOnForwardFailure`: a local port already in use is an error returned from
`net.Listen`, not a flag someone has to remember to set.

Responsibilities:

- Allocate the local port once, at rig creation, and hold it until `down` — including
  across tunnel restarts and instance replacement (FR-WIRE-03, FR-WIRE-07).
- Redial with backoff on connection loss, holding the same local listener throughout.
- **Front the tunnel with a local reverse proxy — not optional.** It is what allows:
  - the port to stay bound (clients see connection refused, not a moved port) while an
    instance is being replaced,
  - the local API key to be enforced (FR-WIRE-08) and the **credential boundary** of
    §15.5.3 to exist at all,
  - request counts and token throughput to be measured for the idle timeout (FR-SUP-06)
    and the TUI, without the control plane touching the data path.

  A bare port-forward could do none of these. The proxy is a component, not a convenience.

The tunnel terminates at `127.0.0.1:<remotePort>` **on the rented host**, which is what makes
§15.5's loopback bind work: the runtime never listens on a routable interface, and the only
way to reach it is through an authenticated SSH channel.

Host key handling is part of this layer, not an SSH detail (§15.4): LARRI keeps its own
`known_hosts` per rig, pins on first connect, and never passes
`StrictHostKeyChecking=no` or `UserKnownHostsFile=/dev/null`.

Provider direct port-mapping is the alternative transport, and it is **off by default**. It
carries the same `Endpoint` contract but not the same security properties: it is plaintext
HTTP on a routable port, so enabling it means the prompts cross the internet in the clear and
the port is reachable by anyone who finds it. Where an operator enables it deliberately, the
runtime API key becomes load-bearing rather than defence in depth, and the choice is recorded
in the journal. It is the one place in this design where the transport choice is *not*
invisible above this layer, which is why it is opt-in.

### 10.2 Client Config Writers

```go
type ClientWriter interface {
    Name() string
    Detect() (bool, error)                       // is this client installed/configured?
    Apply(ep Endpoint, spec ModelSpec) (WiringRecord, error)
    Revert(rec WiringRecord) error
}
```

Protocol, non-negotiable (FR-WIRE-04/05, R-05):

1. **Detect** — never write config for a client that is not present.
2. **Back up** — copy the file, recording the path in `WiringRecord`.
3. **Apply idempotently** — writing twice equals writing once; re-running `up` after a
   crash must not duplicate entries.
4. **Record** — persist what changed *before* the change takes effect.
5. **Revert on `down`** — restore the pre-`up` state exactly. A torn-down rig never leaves
   an IDE pointing at a dead endpoint.

Only the stable loopback URL and the stable served-model name are ever written (P3,
FR-WIRE-06).

#### 10.2.1 Writability Tiers

R-05 is config corruption from automated edits, and the mitigation that matters is not
writing carefully — it is **knowing which clients may be written to at all**. Clients fall
into three tiers, and the tier is a property of the client, declared by its writer:

| Tier | Storage | LARRI's action |
|---|---|---|
| **A — file** | Plain-text config the app re-reads | Full automation: back up, write idempotently, revert byte-exact on `down` |
| **B — app-owned store** | SQLite, keychain, or live process state | **Never write directly.** Use the app's own API if it has one; otherwise demote to tier C |
| **C — guided** | Anything else | Print the exact values, offer to copy them, then *verify by probing* that the client actually reached the endpoint |

Tier B is the one that earns the taxonomy. Editing a SQLite database belonging to a running
Electron app is how you corrupt someone's chat history to save them four clicks. A writer
that cannot honestly promise byte-exact revert must not claim tier A.

Tier C is a real outcome, not a failure. "Here are the two values, paste them here" plus a
probe that confirms it worked is a legitimate integration, and it is infinitely better than a
corrupted config file.

**An environment variable is not evidence of tier A.** This is the trap, and it is worth
naming because the surface looks exactly like tier A: the app documents an env var, LARRI
writes it, and everything appears to work — once. The real test is whether the application
*re-reads* the value or **snapshots it into its own datastore on first run**.

Open WebUI is the canonical example. `OPENAI_API_BASE_URL` is a `PersistentConfig`
variable: read from the environment on first launch, then written to the database, which
wins on every subsequent start. A writer that sets the env var would work on a fresh install
and silently do nothing thereafter — the worst possible failure, because it is invisible.
The escape exists (`ENABLE_PERSISTENT_CONFIG=False` makes the environment authoritative
again), but a writer must *establish* which mode it is in rather than assume.

So the tier is determined by observed reload behaviour, not by the presence of a config
mechanism. When a writer cannot establish it, the answer is tier C.

**Verification by probe applies to every tier, not just C.** A tier-A file that the
application only reads at startup — `librechat.yaml` is one — is correctly written and not
yet in effect. `larri up` therefore probes after wiring, and when the probe fails it says
"restart LibreChat" rather than reporting a success the operator cannot use.

#### 10.2.2 Clients Supported in v1 (Q-01, resolved)

**IDE targets**

| Client | Target | Tier | Notes |
|---|---|---|---|
| **Continue.dev** (VS Code **and** JetBrains) | `~/.continue/config.yaml` | **A** | One file serves both IDEs — `provider: openai` with `apiBase` pointed at the loopback endpoint. The highest-value writer in the set, because a single YAML file covers two of the three requested IDE targets. |
| **VS Code — native Copilot Chat BYOK** | `settings.json`, `github.copilot.chat.customOAIModels` | **A** | Reaches operators who never install Continue. Version-sensitive: the setting has moved during preview, so the writer detects the schema before writing and demotes to tier C when it does not recognise it. |

**Chat targets**

| Client | Target | Tier | Notes |
|---|---|---|---|
| **LibreChat** | `librechat.yaml`, `endpoints.custom[]` | **A** | The primary chat target. A documented, plain-text file whose explicit purpose is registering OpenAI-compatible endpoints — `name`, `baseURL`, `apiKey`, `models.default` — with `${VAR}` substitution from `.env`. Read at startup, so the writer probes and asks for a restart rather than claiming success. |
| **Open WebUI** | `.env` + admin API | **A conditional / B** | Tier A **only** with `ENABLE_PERSISTENT_CONFIG=False`; otherwise `OPENAI_API_BASE_URL` is snapshotted to the database on first run and the env var is thereafter ignored. The writer establishes which mode applies and uses the admin API when persistence is on. Supported because it is the most widely deployed local chat UI, not because it is the cleanest to wire. |
| **AnythingLLM — self-hosted** | `.env` (`GENERIC_OPENAI_*`) | **A** | Retained. Generic OpenAI provider: base URL, key, and an explicitly named model. |
| **AnythingLLM — Desktop** | SQLite in the app's storage directory | **C** | Settings live in `anythingllm.db`. LARRI does not write it, and guided configuration is the honest answer for a desktop app configured once. |

The chat list changed after the tier test was applied honestly. AnythingLLM Desktop was the
original request, and it is retained — but as a guided target rather than the flagship,
because a one-click desktop app that keeps settings in SQLite cannot be automated without
risking exactly the corruption R-05 describes.

**The desktop form factor tends toward tier B or C, and that is not worth fighting.** Electron
chat applications overwhelmingly persist settings in an app-owned store. Rather than pick a
desktop client for its file format — optimising for the wrong property — LARRI covers that
need with its own chat pane (§14.4), and delegates *rich* chat to the deployments that are
genuinely automatable.

**Cursor is deliberately excluded**, and the reason is structural rather than a matter of
effort: Cursor routes model requests through its own backend, so a custom OpenAI base URL
must be reachable *from Cursor's servers*. `127.0.0.1` is not. Wiring it would require
exposing the rig through a public tunnel — which contradicts FR-SEC-03's loopback-only
binding and is listed under Out of Scope as serving a public endpoint to third parties. It is
not that Cursor is hard to support; it is that supporting it means abandoning P3.

JetBrains is therefore served through Continue.dev, which is also why Continue is treated as
the primary target rather than one option among several.

### 10.3 One Rig Now, Many Later (Q-05, resolved)

v1 runs **one rig at a time**, enforced as policy — `max_concurrent_rigs: 1` in config,
checked by the orchestrator, refused with a clear message. Nothing in the architecture
assumes it. State is already keyed by `Rig.ID`, the API already lists rigs, the supervisor is
already one goroutine per rig, and reconciliation already iterates. The limit is a
configuration value, not a shape.

The one place concurrency is genuinely hard is P3, and it is worth designing now because
retrofitting it would rewire every client:

**If clients are wired to a fixed port, which rig is behind it?**

The answer is that `/v1` requests already carry the answer — every OpenAI-compatible request
names a `model`. So the local listener becomes a **router keyed by served-model name**:

```
      client (wired once, forever)
              │
      127.0.0.1:8000/v1     ← the canonical port; never changes
              │
      route on request.model
       ├── "qwen3-coder"  → rig 01J9Z…  → tunnel → host A
       └── "llama3-70b"   → rig 01JA2…  → tunnel → host B
```

With one rig this is a pass-through and costs nothing. With several it means clients are
still configured exactly once, and selecting a rig is choosing a model in a dropdown the
client already has — which is the behaviour an operator would expect without being told.

It also makes the wiring writers (§10.2) forward-compatible without change: they write the
canonical port and a served-model name, which is what they already write. Adding a second rig
adds a model entry, not a reconfiguration.

Per-rig versus global is then a real distinction that v1 must get right even while N=1:
budget ceilings, idle timers, and termination reasons are **per rig**; the budget ceiling
that matters most to an operator — total spend across everything — is **global**. A design
that only ever had one rig would conflate them, and the conflation would be invisible until
the day it was wrong.

---

## 11. State Store and Reconciliation

### 11.1 Layout

```
~/.local/state/larri/
├── rigs/<rigID>.json      current Rig snapshot (atomic writes)
├── journal.jsonl          append-only transition log — never rewritten
├── backups/               client config backups, keyed by rigID
└── daemon.sock            unix socket for the daemon API
```

### 11.2 Durability

Snapshot writes are `write temp → fsync → rename` (atomic on POSIX), so a crash mid-write
leaves the previous valid snapshot intact (FR-STATE-02). The journal is the *authority for
what was attempted*; the snapshot is a convenience view of the current state. When the two
disagree, the journal wins, because the journal is what records intents that never
completed.

Journal entry:

```json
{"ts":"2026-08-21T10:31:02Z","rig":"01J…","from":"SELECTED","to":"CREATING",
 "provider":"vastai","offer":"9182736","price_hr":1.29,"note":"create intent"}
```

Cost accounting is derived from the journal, not from a running counter, so it survives
restarts and remains auditable (FR-STATE-03).

### 11.3 Reconciliation (FR-DEL-05)

Runs on every daemon start, and periodically thereafter.

```
for each configured provider:
    live   := provider.List()                       // ALL instances
    known  := state.RigsFor(provider)

    for inst in live:
        rigID := inst.Labels["larri"]
        switch:
          rigID == "" or not ours   → ignore, it is the operator's own instance
          rigID in known            → adopt: re-attach supervision, rebuild tunnel
          rigID not in known        → ORPHAN: it carries our label but we have no rig
                                       → report loudly; destroy on operator instruction

    for rig in known where rig.State is billable:
        switch on what the provider reports for rig.Instance:
          absent                  → DESTROYED (externally, or by us before a crash)
          present but not running → STOPPED — still billing storage (§12.4)
          present and running     → adopt; if the rig was STOPPED, it has RESUMED:
                                    check for a replacement and surface both
          query failed            → unknown; change nothing, retry (FR-SUP-11)
```

Two failure modes this closes:

- **Crash between intent and create** — the journal shows `CREATING` with no instance ID.
  Reconciliation searches `List` by label. Found → adopt. Not found → the create never
  landed; mark `FAILED` (AC-2.1).
- **Crash after create, before the response was persisted** — identical path. The label is
  what makes this recoverable, which is why `Rig.ID` is minted before the call and stamped
  on the resource (FR-STATE-04).
- **A rig that stopped and resumed while the daemon was down.** The instance is running,
  local state says `STOPPED`, and a replacement may exist. Both carry the label, so both are
  found; the reconciler's job is to surface the pair rather than to pick one, because
  choosing which of two billing instances to destroy is an operator decision (FR-SUP-03).

---

## 12. Supervisor

One goroutine per rig, plus a global reconciler.

| Check | Interval | Action |
|---|---|---|
| Health — real `chat/completions` with a trivial prompt (FR-SUP-01) | 30 s | 3 consecutive failures → `DEGRADED` |
| Instance liveness — `provider.Get` | 60 s | Classify by §12.1. Never "gone" on a failed query |
| Tunnel liveness — child process + local connect | 10 s | Restart with backoff, same local port |
| Cost accrual — elapsed × `PriceHr`, plus storage for `STOPPED` rigs | 60 s | Update; warn at the lead time, then **destroy** on breach (§12.5) |
| Idle — operator inference through the proxy, health probes excluded (§12.2) | configurable | Warn at the lead time, then the configured action |
| GPU presence — `nvidia-smi` supervision probe (§12.3) | 60 s | Device missing → `DEGRADED`, replacement rather than restart |

### 12.1 The Disappearance Taxonomy (FR-SUP-02)

"The rig stopped serving" has at least six causes with six different correct responses, and
an earlier revision of this document collapsed them into two. The classification is resolved
by evidence, never by inference:

| Evidence | Meaning | Billing | Response |
|---|---|---|---|
| Provider reports **not found** | Destroyed — by us, by the host, or by the provider | Ended | Terminal. `DESTROYED`, journal the final cost |
| Provider reports **stopped / exited** | Outbid, host-stopped, or balance exhausted | **Storage, still accruing** | `STOPPED`. Not terminal, not free — §12.4 |
| Running; **SSH refused** | Host networking or `sshd` | Full rate | Retry, then `DEGRADED`. Rebuild the tunnel on the same local port |
| Running; SSH fine; **runtime dead** | OOM at load, crash | Full rate | Restart the runtime in place before considering replacement |
| Running; runtime alive; **no completion** | Wedged model, or the GPU fell off the bus | Full rate | GPU probe (§12.3) decides restart versus replace |
| **Provider unreachable** | Our network, or a provider outage | Unknown | Conclude nothing. Hold, keep supervising, escalate if it persists |

The last row is the one that looks like a gap and is actually the requirement. A failed
query is not evidence of absence, and a supervisor that treats it as such will declare a
rig destroyed during a provider outage while the instance bills happily on. This is
`ErrProviderUnknownOutcome` (§16) arriving on the supervision path rather than the create
path, and it gets the same answer: reconcile, never assume.

### 12.2 Idle Reclamation (FR-SUP-06, FR-SUP-08)

A forgotten rig is the failure this product exists to prevent, so the idle timer is on by
default: `--idle-timeout 30m --idle-action destroy|warn`, overridable per rig and in config.

**What counts as activity is the whole design.** The proxy (§10.1) is already on the data
path and already counting, but a naive count is wrong in a way that silently disables the
feature:

- **LARRI's own health probe must not count.** §12 runs a real `chat/completions` every 30 s
  by design (NFR-05 — readiness must be verified, not assumed). If that traffic resets the
  idle timer, **the timer can never expire**, and a feature that appears to work protects
  nothing. Probes carry an internal marker the proxy recognises and excludes from the
  activity clock while still counting them for health.
- **Non-inference endpoints do not count.** IDE clients poll `/v1/models` to populate menus;
  that is a client being open, not a rig being used.
- **The console pane does not count.** It reads daemon metrics and never touches `/v1`.
- **A request still streaming counts as activity**, however long it has been running. A
  single long generation is the case where "no requests arrived recently" is most misleading.

**`destroy`, not `stop`, is the only reclamation action offered.** Stopping looks like the
gentler option and is the worse one: on Vast.ai a stopped container keeps billing storage —
sometimes at a *higher* rate than while running — and surrenders the GPU with no guarantee
of getting it back. It converts a rig you are not using into a bill you are still paying.

Reclamation runs the full teardown protocol (§13), wiring revert included, so an
idle-destroyed rig never leaves an IDE pointed at a dead endpoint, and it records a
`Termination` with the evidence behind the decision (§13.1) — a rig that disappears while
the operator was away must be able to say why, in a form they can inspect later. The operator is warned
ahead of the deadline on every surface (FR-SUP-09) with time to cancel or extend, because
reclaiming a rig thirty seconds before it was needed is a worse failure than the idle spend
it prevented.

### 12.2.1 Waiting Has Two Regimes

Once SSH is up, LARRI can ask the host directly rather than reading the
provider's status text, and how long to wait should depend on whether there is
anything to be patient about.

**Before the runtime has produced a line**, there is not. Either it died on
launch or the host is doing nothing, and both are answered in a few minutes by
an empty log and quiet hardware. A long deadline here only bills for a failure
already knowable.

**Once output starts**, the calculus inverts. A weight download is legitimately
slow, and killing it discards everything transferred — the mistake a fixed
deadline made three times in a row against a 15 GB image pull. Patience
therefore runs long, and what ends it is a *stall*: no log growth **and** no
hardware movement.

The two signals are independent on purpose:

| Phase | Log grows | CPU | Disk | Network |
|---|---|---|---|---|
| Fetching weights | yes | low | some | **high** |
| Extracting an archive | quiet | high | **high** | idle |
| Loading a checkpoint into VRAM | quiet | high | **high** | idle |
| Genuinely stuck | no | idle | idle | idle |

A growing log proves work when the counters are quiet; busy counters prove work
through a phase that logs nothing. Requiring agreement between them would
reproduce the single-signal blindness these regimes exist to avoid — which is
the same error as judging liveness by network traffic alone, and the reason the
host probe reads CPU, disk and network rather than just the wire.

### 12.2.2 Dead on Arrival Is Common, and Reliability Does Not Predict It

Measured across eleven live rentals on the cheap end of the Vast market:

| | |
|---|---|
| Rentals | 11 |
| Answered SSH at all | 6 |
| **Never answered** | **5** |
| Time-to-answer, when it answered | under 30 s, every time |
| Provider reliability of the failures | 0.94 – 1.00 |

Two things follow, and neither is what a first design would assume.

**Fallback is not an edge case, it is the main path.** A dead-on-arrival rate
near half means an `up` that cannot try another machine will fail about as
often as it succeeds. FR-PROV-05 reads like insurance and is closer to load
bearing.

**Reliability does not predict it.** Every machine that failed scored 0.94 or
better, several scored 1.00. The provider's score describes historical uptime
of a *host*, not whether the container it just scheduled will start — so it is
a useful floor against flaky hardware and no help at all here. What detects
this is the connection (§12.1), and nothing else available does.

The endpoint window follows from the same data rather than from judgement:
every success answered within one poll, so the limit is set at three times the
worst observed success. Waiting longer only bills for machines that were never
going to work.

### 12.3 A Supervision Probe Is Not a Metrics Collector

§17.1's T1 forbids telemetry from influencing supervision, and GPU health is exactly where
someone will be tempted to breach it — the host metrics collector is already running
`nvidia-smi`, so why probe twice?

Because the two have opposite failure semantics. The collector may fail, stall, or be
disabled, and nothing may depend on it. The supervision probe is allowed to decide, so it
must be owned by the supervisor, deadline-bounded by the supervisor, and meaningful when it
fails. They may share the SSH connection; they may not share ownership. **Absence of
telemetry is never evidence of anything** — a rig with metrics collection switched off is
not an unhealthy rig.

The probe answers one question a completion round-trip cannot: is the GPU still there? A
host whose GPU has fallen off the bus keeps `sshd` up and the runtime process alive while
every completion fails, and restarting the runtime in place will fail forever. That is a
replace, not a restart, and only the device query distinguishes them.

### 12.4 Stopped Is Not Gone (FR-SUP-10)

A `STOPPED` rig is the design's sharpest cost trap, because every instinct says the problem
has resolved itself: the GPU is released, the meter appears to have stopped, and the state
name sounds terminal. It is not. The container exists, storage bills per second for as long
as it exists, and the only thing that ends it is a destroy.

So `STOPPED` keeps its supervisor goroutine, stays visible in every surface with its
accruing storage cost, and stays on the reconciler's work list until proven absent. The
default action is to destroy it. Waiting for a resume is available, deliberate, and shows
the operator what the wait costs.

**The resume hazard.** A stopped interruptible can come back on its own when its bid clears
again — "it could be a long wait until it resumes" is a description of a delay, not of a
termination. If LARRI has already provisioned a replacement, that resume produces **two
billing instances**, one of which nobody is watching. Both carry the rig label (§11.3), so
reconciliation finds it; the point is that this is a routine outcome of ordinary preemption
recovery rather than an exotic edge case, and FR-SUP-03's no-silent-upgrades rule is what
keeps a replacement from being provisioned without the operator knowing there is now a
second thing to account for.

**No silent upgrades (FR-SUP-03).** Recovery onto a new instance at a higher price requires
consent. A supervisor that quietly re-provisions at 3× the price while the operator is
asleep is worse than one that stops.

**Daemon death is not rig death (FR-SUP-07).** The instance keeps running and keeps
billing. On restart, the daemon adopts it via §11.3. The tunnel is rebuilt on the same
local port, so clients recover with no reconfiguration.

### 12.5 Budget Ceilings Destroy (Q-03, resolved)

A breached ceiling destroys, matching idle reclamation (Q-11) and for the same reason: the
failure this product exists to prevent is a rig that outlives the operator's attention, and a
warning nobody reads prevents nothing. The same safeguard applies — the teardown records a
`Termination` with `Actor: Policy`, `Code: BudgetCeiling`, and the evidence behind it: the
ceiling, the accrued total, and the sample that crossed it (§13.1).

Two details that follow from the rest of this document rather than from the choice itself:

- **The ceiling must count storage.** A `STOPPED` rig still bills (§12.4). A budget that only
  counts GPU-hours would watch a stopped rig accrue storage charges forever without ever
  reaching a ceiling, which is precisely the leak R-13 describes.
- **Warn with lead time, on every surface** (FR-SUP-09). A ceiling reached mid-generation
  should not sever the response the operator is waiting for; the warning gives them the
  chance to raise it deliberately rather than discover the limit by losing work.

Ceilings are per rig, and there is a global one across all rigs (§10.3). With N=1 they look
identical, which is exactly why both exist now rather than later.

---

## 13. Teardown Protocol

```
0. resolve the termination reason (§13.1) — it is an input to teardown, not a note
   written afterwards
1. state → DRAINING, journal the intent *and the reason*
2. revert client wiring (WiringRecord, reverse order)
3. stop the tunnel, release the local port
4. provider.Destroy(instanceID)
5. VERIFY: provider.Get / List until **absent from inventory**, backoff 1s → 30s,
   deadline 60s. Stopped, exited, and paused are *not* absent (§12.4)
6. absent → state DESTROYED, journal final cost from the transition log
7. not absent within deadline → state remains DRAINING, escalate:
     - persistent warning in every surface
     - non-zero exit from `larri down`
     - the rig stays in the reconciler's work list until proven gone
```

Every step is idempotent and safe to re-enter from any state including `FAILED` and
`ORPHANED` (FR-DEL-02). Order matters: wiring is reverted *before* the instance is
destroyed, so there is no window in which a client is pointed at a dead endpoint.

Step 5 is the requirement that separates LARRI from a wrapper script. A 200 from a delete
endpoint is a claim; absence from `List` is evidence (FR-DEL-03).

Step 0 exists because the reason is only knowable at the moment the decision is made. A
supervisor that destroys first and reconstructs the motive from log lines afterwards will
get it wrong exactly when it matters — when several conditions were true at once.

Two ways step 5 can pass while money keeps leaving, both worth stating because both look
like success:

- **A provider `List` that returns only running instances.** Then a stopped, storage-billing
  container reads as absent and the rig is journalled `DESTROYED` while it bills forever.
  Adapters must enumerate non-running resources, and the provider conformance suite (§18)
  asserts it — the fixture for this test is a stopped instance that must not be reported
  gone.
- **A destroy that stops rather than destroys.** Some provider APIs offer both behind
  similar-looking calls. The adapter must issue the one that removes the resource, and step
  5 is what catches the confusion, provided the check is for absence and not merely for a
  changed status field.

---

### 13.1 Why a Rig Died (FR-DEL-08)

The first question after a rig disappears is always *why*, and the operator should not have
to grep a log to answer it. Every teardown records a `Termination`, and it survives the rig.

**The `Actor` axis carries most of the meaning**, because it maps to the question actually
being asked:

| Actor | Meaning | Example codes |
|---|---|---|
| `Operator` | You asked for this. | `OperatorRequest`, `PanicSweep` |
| `Policy` | A rule *you configured* fired. | `IdleTimeout`, `BudgetCeiling` |
| `Provider` | It was done to you. | `Preempted`, `HostFailure` |
| `Fault` | LARRI could not continue. | `ProvisionDeadline`, `BootstrapFailed` |

"Did I do this, did my own settings do this, did they do this, or did the tool break" is a
different question from "which code fired", and it is the one that determines whether the
operator changes a flag, changes provider, or files a bug.

**A reason without evidence is not an explanation.** `IdleTimeout` alone invites the
suspicion that the timer misfired. The evidence is what closes that:

```console
$ larri status 01J9Z…
  rig 01J9Z…  DESTROYED   ran 2h14m · total $2.87
  ended       2026-08-21 14:22:07  ·  policy: idle-timeout
              no operator inference for 31m (window 30m)
              last request 13:51:04 · 1,204 requests over the rig's life
              health probes excluded from the activity clock
  reclaimed   $0.64/hr of idle spend avoided
```

The line about probes being excluded is there deliberately. The operator who wonders whether
the 30-second health check should have counted as activity (§12.2) gets the answer without
having to ask, and the one who suspects a misfire can see the last real request.

Per-code evidence, at minimum:

| Code | Evidence |
|---|---|
| `IdleTimeout` | last operator request, configured window, actual idle duration, lifetime request count |
| `BudgetCeiling` | ceiling, accrued, the sample that breached it |
| `Preempted` | provider status observed, when, interruptible flag, whether storage still bills |
| `HostFailure` | phase reached, error class, which offer was next |
| `ProvisionDeadline` | deadline, elapsed, phase reached |
| `OperatorRequest` | which surface issued it |

**Retention is the other half of "inspectable".** A `DESTROYED` rig that vanishes from
`larri status` has explained nothing, so terminated rigs are retained and listed — most
recent first, with `larri status --all` for the full set. Snapshots age out by count and by
age, both configurable; the journal (§11.2) is append-only and remains the permanent record
regardless of snapshot retention, which is why cost history survives even after a rig's
snapshot is pruned.

The reason appears in every surface: `larri status` and `larri down`'s exit output, the SSE
event stream, the TUI, the console pane's money row, and the `larri_status` tool result —
where FR-SEC-06 redaction applies, since that result travels back to the untrusted host.

## 14. Daemon API and Surfaces

### 14.1 Daemon API

HTTP over the unix socket at `~/.local/state/larri/daemon.sock`. Loopback-equivalent by
construction, so filesystem permissions are the access control.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/rigs` | Create a rig (search → rank → provision → bootstrap → wire). Streams progress via SSE. |
| `GET` | `/v1/rigs` | List rigs and states. |
| `GET` | `/v1/rigs/{id}` | Rig detail, cost, health, endpoint. |
| `DELETE` | `/v1/rigs/{id}` | Teardown (§13). |
| `GET` | `/v1/rigs/{id}/logs` | Runtime logs, streamed. |
| `POST` | `/v1/offers` | Search + rank only. No spend. |
| `GET` | `/v1/orphans` | Reconciliation report. |
| `DELETE` | `/v1/orphans/{provider}/{id}` | Destroy an orphan. |
| `GET` | `/v1/events` | SSE stream of state transitions — the shared feed behind FR-UI-06. |
| `GET` | `/v1/metrics/stream` | SSE stream of 1 Hz sample frames — the feed behind the console pane and the TUI's live panels. |
| `GET` | `/v1/metrics/history` | Ring-buffer backfill so a chart renders populated on load (`?window=15m`). |

Every surface consumes `/v1/events`, which is how all four stay consistent within one
health-check interval without polling.

### 14.2 Tool Surface — One Registry, Two Drivers

LARRI's operations are exposed as tools to two different drivers: an external agent over
MCP (Claude Code and friends), and the model on the rig itself through the chat pane
(§14.4.4). Both consume **one registry** in `internal/tools` — name, description, JSON
schema, consequential flag, handler — adapted per driver:

```
internal/tools  (canonical: schema + handler + consequential flag)
        ├── internal/mcpsrv   → MCP tool definitions      → Claude Code, other agents
        └── internal/webui    → OpenAI `tools[]` schemas  → the model on the rig
```

Two adapters over one definition, never two definitions. A `larri_down` that means something
different depending on who called it is the failure mode this structure exists to prevent,
and it is P1's argument applied to the tool surface.

| Tool | Consequential | Exposure | Notes |
|---|---|---|---|
| `larri_search_offers` | no | both | Criteria in, ranked offers out. Safe to call freely. |
| `larri_plan` | no | both | Sizing plan and cost estimate for a model. |
| `larri_status` | no | both | Current rigs, cost accrued. |
| `larri_logs` | no | both | Runtime logs. |
| `larri_metrics` | no | both | Recent samples from the ring buffer (§17.5) — what the console pane graphs, as numbers. |
| `larri_orphans` (list) | no | both | Reconciliation report. |
| `larri_up` | **yes** | MCP; chat pane opt-in | Starts spending. Returns the estimated $/hr in the result. |
| `larri_down` | **yes** | MCP; chat pane opt-in | Stops spending. Returns total cost. |
| `larri_orphan_destroy` | **yes** | MCP; chat pane opt-in | Split from the list tool so the read is always safe. |

Consequential tools state the cost implication in their result text so the driving agent can
report it rather than infer it (FR-UI-03). The `Exposure` column is the load-bearing
addition: the read-only set is identical for both drivers, while the consequential set is
opt-in for the chat pane for the reasons in §14.4.4.

### 14.3 TUI

Bubble Tea: offers table with score breakdown, provisioning progress with per-phase bytes,
live health, tokens/sec, elapsed time, accrued cost, and a destroy confirmation. Reads
`/v1/events` and `/v1/metrics/stream`; holds no state of its own (P5).

### 14.4 Web UI — Two Panes, Two Connections

An earlier revision required the web UI to be an ordinary consumer of `/v1`, and held that
needing a path into the daemon meant the endpoint contract was wrong. Both panes now need
such a path — the console to draw graphs, the chat pane to execute the tool calls the model
emits (§14.4.4). The principle survives, but it has to be stated about *traffic* rather
than about panes:

> **Inference flows only over `/v1`. Actions flow only through the tool registry.**
> Neither pane may invent a third path. The console reads the daemon's event and metric
> streams; the chat pane executes tools that an external agent could equally invoke over
> MCP. A pane that needs a bespoke daemon endpoint of its own is the signal that the
> contract is wrong.

That preserves what the original was protecting — `/v1` stays sufficient, and pure, for
anything that serves tokens — while admitting two surfaces that were never chat clients.

| Pane | Inference | Actions / data |
|---|---|---|
| Chat | `127.0.0.1:<localPort>/v1`, OpenAI-compatible SSE, identical to any other client | tool registry (§14.2), same definitions MCP exposes |
| Console | none | `/v1/events` + `/v1/metrics/stream` (SSE), `/v1/metrics/history` (backfill) |

#### 14.4.1 Console Pane Contents

| Row | Panels | Source |
|---|---|---|
| **Money** | state badge, uptime, $/hr, accrued $, projected $ at 1 h / 8 h / 24 h, budget-ceiling gauge | journal (§11.2) |
| **Rig** | GPU utilisation per GPU, **VRAM used/total with the sizing plan's requirement drawn as a reference line**, temperature, power, host CPU, RAM, disk, network | host collector (§17.4) |
| **Inference** | tokens/sec, TTFT p50/p95, requests in flight, queue depth, KV-cache utilisation, context occupancy | runtime scrape + local proxy |
| **Local** | daemon CPU/RAM, tunnel state, consecutive health-check results | daemon collector |

The money row comes first because it is the row nothing else provides. GPU and VRAM graphs
are available from any monitoring stack; a live accrued-cost figure tied to the rig
producing it is the reason this console exists rather than a Grafana dashboard.

#### 14.4.2 Serving and Access Control

The daemon API lives on a unix socket (§11.1) and a browser cannot open one. `larri ui`
therefore starts a loopback HTTP listener that bridges to the socket — which trades
filesystem permissions for a network boundary, and that trade has to be paid for:

- Bind `127.0.0.1` only, never a routable interface.
- Mint a session token per `larri ui` invocation, deliver it in the URL that is opened, and
  require it on every daemon-API request. Socket permissions no longer protect anything
  reachable through this listener.
- Validate `Origin` and `Host` on every request. A loopback listener that checks neither is
  reachable from any page the operator has open, by DNS rebinding — which is the specific
  reason an unauthenticated localhost admin port is not the safe default it appears to be.
- Proxy `/v1` through the same listener so the chat pane is same-origin and no CORS
  configuration is needed. The canonical `127.0.0.1:<localPort>/v1` that real clients were
  wired against (P3) is untouched; this hop is a browser convenience, not a moved endpoint.

#### 14.4.3 Stack

Go, and no Node — which resolves Q-02. Stdlib `html/template` renders a static shell,
`go:embed` ships it, vanilla JS drives it, and **uPlot** (~48 KB, MIT) draws the charts.

The reasoning is not a preference for Go over React; it is the shape of the problem. A KPI
console is a 1 Hz data feed driving imperative canvas redraws — `EventSource` in,
`chart.setData()` out — which is precisely what server-rendered hypermedia is worst at.
HTMX supplies no reactivity, and Datastar, though SSE-native with a Go SDK, is pre-1.0;
either would be carried for a fragment-swap model this console never uses.

The remaining candidate — a React build around AI SDK 6 and AI Elements — was rejected on a
harder point than taste. `useChat` speaks the AI SDK UI message stream protocol, not raw
OpenAI SSE, so its intended architecture puts a Node server route between the browser and
the model. That is a runtime dependency NFR-06 forbids. A static build with a hand-written
`ChatTransport` would avoid the Node runtime, but it discards the half of the SDK that
justifies adopting it, and still brings a second toolchain and an npm dependency tree to
audit under NFR-10 — all for components in a pane that is deliberately minimal.

Minimal as a *chat client* remains the point — Open WebUI, LibreChat, and the operator's IDE
are better at that, and wiring them to the rig is FR-WIRE's job. But the pane is not
competing with them on chat. It exists for something they cannot do without configuration:
put LARRI's own tools in front of the model the rig is serving, so the operator can ask the
rig about itself (§14.4.4). That capability, not message rendering, is what justifies
shipping a chat surface at all.

Desktop toolkits were never candidates. Wails, Fyne, and Gio each require CGO, and §19
requires `CGO_ENABLED=0` cross-compilation.

#### 14.4.4 Chat-Driven Control

The chat pane's purpose is that the operator can drive and debug LARRI by talking to the
model the rig is already serving — "why did the last rig go degraded", "what is my accrued
cost", "show me the tail of the runtime log". The pane advertises the read-only tools from
§14.2 on every request and executes what comes back:

```
operator ─► pane ─► POST /v1/chat/completions  { messages, tools[] }
                          │
                    model emits tool_calls
                          │
         pane ─► internal/tools handler ─► daemon API
                          │
              result appended as a tool message
                          │
                    POST /v1/chat/completions  (loop, bounded)
                          │
                    final assistant message ─► operator
```

This is the same registry Claude Code drives over MCP (§14.2). The difference is only *who
is emitting the calls*, and that difference is the entire security problem.

**The model runs on hardware LARRI does not trust.** FR-SEC-05 already establishes the
rented host as a third party's machine. A model served from it is therefore an untrusted
source of tool calls, and three distinct things can put a hostile call on the wire: a
compromised host returning whatever completion it likes; prompt injection, when the operator
pastes a log line or a web page into the conversation; and ordinary hallucination by a small
quantized model. None of these is exotic, and the first is *inherent* to renting GPUs from
strangers rather than a failure that has to occur first.

The controls follow directly:

1. **Read-only tools auto-execute. Consequential tools never do.** `larri_up`, `larri_down`,
   and `larri_orphan_destroy` are excluded from the chat pane's tool set by default. Where
   the operator opts in, a tool call renders a **confirmation card** stating the operation,
   the instance, and the cost implication, and nothing happens until the operator clicks.
   This is §14.2's existing stance for MCP, applied unchanged — a consequential operation is
   confirmable regardless of which driver proposed it (FR-UI-03).
2. **The auto-executed set cannot spend, destroy, or rewire.** That is the definition of the
   default set, not a property to be verified per tool. A new tool joins it only by being
   incapable of those three things.
3. **Tool results are redacted before they go back to the model.** This is subtle and it is
   the one that will be got wrong: the tool result is appended to the conversation and
   **posted to the untrusted host** on the next turn. `larri_logs` in particular can carry a
   token echoed into a runtime log line, and returning it to the model ships it straight to
   the host it should have been kept from. FR-SEC-02's redaction therefore covers a fifth
   destination here, and it is the only one where the recipient is untrusted by design.
4. **The loop is bounded.** A maximum tool-call depth per turn, and a cap on result size.
   An untrusted model that emits tool calls forever must exhaust a budget, not the daemon.

**The bootstrap paradox, and its resolution.** The assistant is least available exactly when
it is most wanted: if the rig is `DEGRADED`, `STOPPED`, or `DESTROYED`, there is no model to
ask.

**The pane is disabled when no rig is serving** (Q-09, resolved). It states why, and it does
not fall back to another endpoint — not a local Ollama, not a hosted API. The reasoning is
that a fallback would quietly change who is being talked to at the moment the operator is
least able to notice, and would drag a second inference dependency into a tool whose premise
is that you brought your own. A disabled pane that says "no rig is serving" is more honest
than a working one that has silently become something else.

Nothing is lost by this, because the two surfaces that matter during an outage do not depend
on the rig: the console pane is fed by the daemon, and the MCP surface lets Claude Code
diagnose a rig that is not serving. That asymmetry is the argument for **MCP remaining
primary for control** — external agents work when the rig does not, and the chat pane
structurally cannot.

---

## 15. Configuration, Secrets, and Security

### 15.1 Configuration

`~/.config/larri/config.yaml` for non-secret settings: enabled providers, default criteria
profiles, ranking weights, local port, budget defaults, client wiring targets,
`max_concurrent_rigs`, idle and budget policy, and the enabled client writers.

#### 15.1.1 Zero Config Is a Supported Mode, Not a Degraded One

**A machine that has never run LARRI can run `larri up --model <ref>` successfully.**
FR-CRIT-04 already requires that invocation to be valid on its own, which settles the
question: configuration is an optimisation over defaults, never a prerequisite. A tool whose
promise is two commands cannot have a third that must run first.

Values resolve in this order, and every layer is optional:

```
flags  →  named profile (--profile)  →  config file  →  built-in defaults
```

Secrets are the exception and resolve separately (§15.2), because a missing API key is not
something a default can supply.

#### 15.1.2 First Run Is Not Interactive by Default

The tempting design is a first-run wizard — a TUI that asks for criteria and writes a config.
It is the wrong answer here for three reasons, and the third is the one that would have bitten:

1. **Ordering.** The TUI is FR-UI-04, priority S, landing in M4. `larri up` is FR-UI-01,
   priority M, landing in M1. A first-run flow that depended on the TUI would leave three
   milestones with no first run at all.
2. **P5.** Surfaces are clients, not owners. A TUI that creates and owns configuration is
   lifecycle logic living in a surface, which is the thing P5 exists to prevent. Config
   creation belongs to `internal/config`; a surface may *drive* it and must not *be* it.
3. **`larri_up` is an MCP tool.** An agent — Claude Code, or anything else driving the MCP
   surface — can trigger a first run. So can CI, a script, and `larri daemon` started by
   systemd. A blocking interactive prompt on a code path an agent can reach is not a prompt;
   it is a hang, with no output and nothing able to answer it.

So interactivity is **detected, never assumed**. LARRI prompts only when all of these hold:

- stdin *and* stderr are terminals,
- `--non-interactive` / `--yes` was not passed and `LARRI_NON_INTERACTIVE` is unset,
- the process is not the daemon, the MCP server, or a `--json` invocation.

Otherwise it proceeds on defaults and prints what it assumed. `CI=true` in the environment
is treated as non-interactive, because it almost always is.

#### 15.1.3 Destructive Defaults Are Stated Even When Nothing Is Asked

Two defaults destroy things: idle reclamation and budget breach both default to `destroy`
(Q-11, Q-03). Those defaults are correct — forgetting is the failure this product
exists to prevent — but a default that destroys and was never mentioned is a trap, however
well reasoned.

So on the first run that would create a config, LARRI **prints the destructive defaults
whether or not it is interactive**:

```console
$ larri up --model Qwen/Qwen3-Coder-30B
  first run   writing ~/.config/larri/config.yaml
              idle-timeout   30m → destroy   (larri config set idle.action warn)
              budget         none set        (larri config set budget.max 5.00)
              providers      vastai (VASTAI_API_KEY found)
              wiring         continue.dev detected
  …
```

Non-interactive runs get the same four lines. Interactive runs get the same four lines and a
chance to change them. The information is not the reward for being at a terminal.

#### 15.1.4 Criteria Are Saved Explicitly, Never Silently

Named profiles (FR-CRIT-05) are how criteria are reused. What LARRI does **not** do is
remember the last invocation and apply it to the next bare `larri up`, and the reason is
cost: a bare command that silently reuses whatever was typed a fortnight ago can provision an
H100 because that is what the last experiment needed. Explicit is cheap; implicit is
expensive and arrives as a surprise.

After a successful `up`, the criteria used are printed with the command that would save them:

```console
  ✓ rig 01J9Z… READY   http://127.0.0.1:8000/v1
    reuse these criteria:  larri profile save coder
```

The TUI's genuine role here is the one it is actually good at — **exploring** offers
interactively, watching the ranking respond as criteria change, and saving the set converged
on (`larri offers --tui`, then save). That is FR-UI-04's live-offers dashboard doing what it
was already for, and it stays a client: it calls the daemon API to persist a profile rather
than writing config itself.

### 15.2 Secrets

Secrets resolve in order: environment (`VASTAI_API_KEY`, `RUNPOD_API_KEY`, `HF_TOKEN`) →
OS keyring → interactive prompt. Never a config file, never state, never the journal
(FR-SEC-01).

A `Secret` string type with `String()`/`MarshalJSON()` returning `"***"` makes redaction
structural rather than a discipline that has to hold at every log site (FR-SEC-02).

The Hugging Face token is passed to the remote host as a process environment variable for
the download, never written to a file there (FR-RT-03) — the host is untrusted (FR-SEC-05).

### 15.3 Threat Model

Security decisions are only checkable against a stated set of principals, so here is the set.

| Principal | Trust | Consequence |
|---|---|---|
| The operator's user account | **Trusted.** The TCB. | If this is compromised, nothing below matters. |
| Other local users and processes | **Untrusted.** | `127.0.0.1` is *not* a per-user boundary — every local user can reach a loopback port. |
| Web content in the operator's browser | **Untrusted.** | Any open page can issue requests at loopback ports. |
| The network path | **Untrusted.** | Assume interception and modification everywhere. |
| The provider | **Trusted for control *and, under proxy SSH, for transport integrity*.** | It says what exists and what it costs, and it supplies the host key fingerprint LARRI pins — so it cannot be excluded from the transport by pinning against a key it provided. Not trusted with workload confidentiality. See §15.8.2. |
| The rented host | **Untrusted, and it has root.** | The hard boundary. §15.7 states plainly what this costs. |
| The model served from that host | **Untrusted output** (§14.4.4). | Its tool calls are attacker-controlled input. |

### 15.4 Transport Security

**Provider control plane.** HTTPS with full certificate verification, which is never
disabled, not even behind a debug flag — a flag that disables verification is a flag that
ends up set in someone's shell profile. API keys travel in headers, never in query strings,
where they would be captured by every intermediary's access log.

**Data plane: the SSH tunnel, and host key pinning is mandatory.** The tunnel is the only
default path to the runtime, and its security rests entirely on knowing which host answered.
So:

- LARRI maintains **its own `known_hosts`, per rig**, and never touches the operator's.
- The key is pinned on first connect and its fingerprint journalled, making a later change
  auditable rather than merely surprising.
- `StrictHostKeyChecking=no` and `UserKnownHostsFile=/dev/null` are never emitted. They are
  the standard workaround for rented boxes whose keys rotate, and they disable exactly the
  protection this section is about.
- **A host key that changes mid-rig is treated as a compromise, not an inconvenience**: the
  tunnel does not reconnect, the rig goes `DEGRADED`, and the operator is told. A rented
  instance's key changing under a live rig has no benign explanation.

This is why FR-SEC-04 is mandatory rather than a nice-to-have. First-connect pinning does
leave a TOFU window, and it is genuinely narrow — LARRI learns the endpoint from the provider
API over verified TLS moments before connecting — but it is not zero, and calling it zero
would be dishonest.

Note that Vast.ai's default proxy connection puts the provider on the SSH path. That is
consistent with the table above: the provider is trusted for control and not for workload
confidentiality, and §15.7 explains why this changes less than it appears to.

### 15.5 The Local Surface

**On the rented host, the runtime binds loopback, and this is not configurable.** A bind
address is computed by the `Runtime` implementation, never taken from operator input, and the
launch path *rejects* any non-loopback value rather than warning about it. There is no config
key, no flag, and no escape hatch, because the only reason to want one is the reason this
rule exists.

An inference server bound to every interface on a rented box is reachable by anyone who scans
for it: no login screen, your GPU, your bill. Talos found over 1,100 exposed Ollama servers in
the wild, and vLLM's own guidance is to expose the minimum surface. On Vast.ai the exposure is
sharper than it first appears, because public IPs are **shared between tenants** — the address
your instance answers on is one other people's instances answer on too.

The SSH tunnel already forwards to the remote loopback (§10.1), so binding there costs nothing
and closes the entire class. The runtime additionally runs with its own API key, so a port
that somehow became reachable is still not an open one.

#### 15.5.1 Provider-Side Isolation

**Neither Vast.ai nor RunPod offers a security group.** There is no platform-level firewall,
no inbound IP allow-list, nothing resembling an AWS security group. Vast's own security FAQ
says network restrictions "depend on the host configuration" and points users at
container-level firewall rules; IP allow-listing is possible only by choosing hosts that
happen to have static IPs, which is a property of the host rather than a control the platform
offers.

So the provider-side mechanism for dropping everything that is not the tunnel is **the
absence of a port mapping** — and on these platforms that is not a workaround, it is the real
control, and it is stronger than a firewall would be:

| Property | Port mapping withheld | Host firewall |
|---|---|---|
| Enforced by | The provider, upstream of the host | The host, which the tenant does not trust |
| Can the host undo it? | No | **Yes — it has root** |
| Needs privilege in the container? | No | `NET_ADMIN` |
| Fails open or closed? | Closed — nothing was ever routed | Open, if the rules fail to apply |

RunPod states this outright: to prevent public accessibility, remove all exposed HTTP and TCP
ports. That is FR-SEC-15, and it is already the design.

**Vast confirms containers are unprivileged**, which settles the earlier firewall question
rather than leaving it as an inference: `NET_ADMIN` is not available in an unprivileged
container, so in-container `iptables` was never going to work on Vast at all.

**RunPod's proxy is a hazard worth naming, because there declaring a port *publishes* it.**
An HTTP port declared on a pod is automatically served at
`https://[POD_ID]-[PORT].proxy.runpod.net` through Cloudflare, and RunPod's documentation is
explicit that the Pod ID is obscurity rather than security and that the application must
supply its own authentication. A runtime port declared in a RunPod template is therefore an
inference endpoint published to the internet. LARRI declares SSH and nothing else — and the
loopback bind (FR-SEC-08) is what makes even a mistaken declaration harmless, since the proxy
would reach a port with nothing listening on it. This is the one place where the redundancy
between two controls has a concrete payoff rather than being belt-and-braces on principle.

**Zero exposed ports is achievable where the provider proxies SSH itself.** Both platforms
offer a proxied SSH path that needs no public port — Vast's `ssh4.vast.ai`, RunPod's
`ssh.runpod.io` with key injection. Where that path carries port forwarding, LARRI can run a
rig with *no* provider port mapping whatsoever, which is the strictest possible answer to this
question. RunPod's proxy is documented as not supporting SCP/SFTP and its forwarding support
is not documented either way, so the adapter **determines this empirically at bootstrap and
records what it found**, falling back to a mapped SSH port when forwarding is unavailable.
Guessing here would either give up isolation that was available or produce a rig with no
usable data plane.

One further control is a *selection* criterion rather than a network one: Vast markets
certified providers (Tier 3/4 datacentres, ISO 27001) and recommends them for sensitive
workloads. That belongs in `Criteria` as a filter, not in the wiring layer, and it changes who
the untrusted host is rather than what it can reach.

#### 15.5.2 SSH Carries the Whole Boundary

Because the tunnel is the sole path, its configuration is a security control rather than a
connection detail — so LARRI **speaks SSH in-process** via `golang.org/x/crypto/ssh` rather
than driving an `ssh` binary.

The reason is that most of the hardening below stops being configuration and becomes
structure. An option you must remember to pass can be forgotten, overridden by the operator's
`~/.ssh/config`, or silently unsupported by whichever `ssh` build is on the machine; a
capability that was never implemented cannot be any of those things.

| Control | In-process | Would have been |
|---|---|---|
| **Agent forwarding** | **Not implemented.** No agent is ever contacted | `-o ForwardAgent=no`, defeatable by `~/.ssh/config` |
| X11 forwarding | Not implemented | `-o ForwardX11=no` |
| Remote forwarding, SOCKS | Not implemented | flags |
| Operator's `~/.ssh/config` | Never read | `-F /dev/null` |
| Host key | `HostKeyCallback` compares against the pinned key; a mismatch is an error return | `StrictHostKeyChecking` + a `known_hosts` file |
| Failed local forward | `net.Listen` returns an error before readiness | `-o ExitOnForwardFailure=yes` |
| Authentication | `ssh.PublicKeys` only; no password method is offered | `-o PasswordAuthentication=no` |
| Local bind | `net.Listen("tcp","127.0.0.1:…")` — explicit | `-L` prefix plus `GatewayPorts` defaults |
| Algorithms | `ssh.Config` names the permitted KEX, ciphers, and MACs | `-o Ciphers=…` triplicated |
| Keepalives | `SendRequest` keepalive on the connection, feeding §12's 10 s check | `ServerAliveInterval` |
| Multiplexing | Native — channels on one connection for tunnel, exec, logs, and metrics (§17.4) | `ControlMaster` and a socket on disk |

Agent forwarding is the row that justifies the rest. A forwarded agent socket lets a host with
root authenticate **as the operator** to every system that trusts their key, and the usual
defence is a flag that a single line in a personal `~/.ssh/config` can override. Not
implementing the capability removes the question.

This is also what lets NFR-06 tighten from "no runtime dependency beyond an `ssh` client" to
**no runtime dependencies at all**.

**The ephemeral per-rig key.** Public keys are not secret, so uploading one is not a
disclosure — but reusing the operator's identity across rented hosts means every host learns
the same public identity, and access outlives the rig unless someone remembers to clean
`authorized_keys` on a machine they no longer rent. A fresh ed25519 pair per rig makes
revocation automatic: destroy the rig, discard the key, and the credential that could reach it
stops existing. The private key never leaves the local machine.

**SSH is now a single point of access, and that is an accepted trade.** If sshd on the host
dies, the data plane is gone — no fallback path exists by design, because every fallback path
is also an attack surface. Supervision detects it (§12.1), the rig goes `DEGRADED`, and
recovery is replacement rather than a second door. Teardown is unaffected: destroying is a
provider API call and has never depended on SSH (FR-SEC-18), so losing access can never
produce an instance that bills forever.

**On the operator's machine**, three listeners exist and each is deliberately constrained:

| Listener | Bind | Control |
|---|---|---|
| `daemon.sock` | unix socket | `0600` in a `0700` directory; the daemon refuses to start on wider permissions rather than fixing them silently |
| `127.0.0.1:<localPort>/v1` | loopback | **API key required, not optional** (FR-WIRE-08); `Host` validated |
| `larri ui` bridge | loopback | per-invocation session token; `Origin` and `Host` validated (§14.4.2) |

The local API key is mandatory because of two rows in §15.3. Loopback is not a user boundary,
so on a shared machine every local account can reach the endpoint. And a page in the
operator's browser can issue requests at loopback ports — CORS governs whether the *response*
is readable, but the request still fires, and for LARRI a request that fires is a request that
spends. `Host` header validation closes the DNS-rebinding variant of the same attack.

The key is written into client config files (§10.2), which makes those files' permissions
part of this boundary, and makes removing the key part of `Revert` rather than an
afterthought.

#### 15.5.3 Authenticating Inference Requests

Two credentials, both generated by LARRI, never operator-supplied, and with **deliberately
opposite lifetimes**:

```
 client                 LARRI proxy                    rented host
   │                         │                              │
   │── Authorization: ──────▶│                              │
   │   <client token>        │  verify, then STRIP          │
   │                         │                              │
   │                         │── Authorization: ───────────▶│  runtime
   │                         │   <rig token>   (in tunnel)  │  verifies
```

| | Client token | Rig token |
|---|---|---|
| Guards | The local `/v1` listener | The runtime on the rented host |
| Lifetime | **Stable across rigs** | **Ephemeral, one per rig** |
| Known to | The client's config file, and LARRI | LARRI and that one host |
| Rotated when | The operator revokes it | Every rig, and every instance replacement |

The opposite lifetimes are forced by P3. A client token that changed per rig would mean
rewriting every client config on every teardown — precisely the churn the stable local
endpoint exists to prevent. A rig token that persisted across rigs would mean a credential
outliving the host it was issued for, which is the property that makes rented hardware
dangerous.

**The proxy strips the client's `Authorization` header and substitutes the rig's.** This is
the rule most likely to be got wrong, because the default behaviour of nearly every reverse
proxy is to forward headers unchanged — which would send the token shared by all of the
operator's IDEs straight to an untrusted host. Nothing the client presents is forwarded, and
nothing the rig requires is ever visible to a client.

That makes the proxy a **credential boundary**, and it buys two properties worth naming:

- A leaked client config discloses a token that reaches only the operator's own loopback
  listener, never a rig.
- A compromised host learns a token that dies with that rig, and never one that any client
  is configured with.

**Per-client tokens.** The local side issues a *distinct* token per wired client — Continue,
VS Code, LibreChat — rather than one shared secret. Three things follow, and the third is the
reason to bother: one client can be revoked without rewiring the others; a leaked config file
burns exactly one credential; and requests carry an identity, so the console's cost figures
can be attributed **per client**. "Which of my tools spent this" is a question only LARRI is
positioned to answer, and it costs nothing extra once tokens are per-client rather than
shared.

**The browser never holds a token.** The chat pane's `/v1` calls go through the same-origin
proxy on the UI listener (§14.4.2), which injects the credential server-side. A token in
page JavaScript would be readable by anything that achieved script execution there.

Mechanically: tokens are 256-bit random values, compared in constant time, typed `Secret` so
`String()` and `MarshalJSON()` redact them (§15.2), held in state and never in the journal,
and removed from client configs by `Revert` (§10.2).

**Why not something stronger than a bearer token inside the tunnel?** Because it would defend
against nobody. The channel is already authenticated and encrypted by SSH, terminating on a
loopback-bound service. The only principal positioned between the proxy and the runtime is the
host itself — which has root, and can read the runtime's memory rather than bother with its
API. Request signing, mTLS, or nonce schemes inside that channel would add machinery and
change no outcome. The rig token earns its place as a *fail-safe* — it is what still stands if
a port is someday mapped by mistake or a bind address is someday wrong — not as a defence
against an adversary already inside the tunnel.

#### 15.5.4 Sealing the Ownership Marker

The marker LARRI stamps on a rented resource is read by the host and by the
provider, so its descriptive fields are encrypted: AES-256-GCM, random nonce per
write, under a 32-byte key the operator supplies via `LARRI_LABEL_KEY` or a file
it points at.

**The key is configuration, never generated silently.** A key LARRI invented and
stored by itself would be one the operator does not know exists, cannot back up,
and loses on reinstall — at which point every surviving rig's details become
unreadable in precisely the situation where an orphan most needs explaining. A
key they supplied is a key they can keep.

**The rig ID is outside the seal, deliberately.** It is the difference between
"this instance is yours" and "this instance belongs to someone" — and that
distinction has to work with no key at all, on a fresh machine, during the
recovery where local state was what went missing. Sealing it would trade a
cost-safety guarantee for hiding an opaque identifier the provider already
associates with the operator's account.

**Nonces are per write**, so two rigs serving the same model do not produce
identical markers, which would otherwise let a host correlate them.

When no key is configured, markers are written unsealed and LARRI says so. It
does not quietly fall back to protecting nothing while implying otherwise.

### 15.6 Denial of Service Is a Financial Attack

For most services a DoS costs availability. For LARRI it costs money, which changes what the
mitigations are aimed at: an attacker reaching your endpoint does not want your data, they
want your GPU-seconds, and the existing cost-safety machinery is most of the defence.

| Vector | Control |
|---|---|
| Unauthorised inference against the rig | Loopback bind on the host, mandatory local API key, runtime API key |
| Authorised-but-runaway spend | Budget ceilings that destroy (§12.5), idle reclamation (§12.2) |
| Request flooding through the proxy | Per-client rate limit and concurrency cap, max-tokens ceiling per request |
| The host stalling the tunnel — slow-loris, silence | Request deadlines, response size caps, health probe → `DEGRADED` |
| An untrusted model emitting tool calls forever | Bounded loop depth and result size (§14.4.4) |
| Provider API rate limiting | Backoff, and reconcile-before-retry for mutations (§16) |

The proxy is the right place for the first three because it is already on the data path and
already counting for the idle timer (§10.1).

### 15.7 What LARRI Cannot Protect You From

Every control above defends the network path and the local machine. None of them defends
against the host, and it would be dishonest to imply otherwise.

**The host operator has root.** They can read process memory, `/proc/<pid>/environ`, and GPU
memory. Every prompt, every completion, the weights, and the Hugging Face token used to fetch
them are visible to whoever owns the machine. No transport security changes this, because the
data must be plaintext at the point of inference. This is FR-SEC-05 and R-06 stated as an
engineering fact rather than a caveat.

What can be done is limiting the blast radius:

- **Least-privilege, short-lived Hugging Face tokens** — fine-grained, read-only, scoped to
  the repositories a rig needs. A rented host learning a token that can read one public
  repository is a different event from one learning a token that can write to an
  organisation.
- **Never reuse credentials across rigs**, so disclosure is scoped to one host.
- **Provider API keys never reach the host at all.** The host can spend your GPU-seconds; it
  must never be able to create instances.

The only real answer to the confidentiality problem is confidential computing — H100 CC mode,
SEV-SNP — where memory is encrypted against the host and attestable. It is not available on
commodity marketplace hardware, and pretending otherwise would be worse than saying so.

So the operative control is a decision, not a mechanism: **do not send a rented GPU anything
you could not afford to disclose.** LARRI's job is to make that decision an informed one, by
never obscuring which host is serving and what it can see.

### 15.8 Attack Surface Analysis

What follows is organised by **where the attacker sits**, because that determines what they
can reach far more than what they intend. Status is honest: *Mitigated*, *Partial*,
*Accepted* (understood and deliberately not defended), or **Open** (a real gap, listed so it
is chosen rather than discovered).

#### 15.8.1 The Host Operator — root on the machine you rented

| Attack | Status | Note |
|---|---|---|
| Read prompts, completions, weights from process or GPU memory | **Accepted** | Unfixable without confidential computing (§15.7) |
| Read `HF_TOKEN` from `/proc/<pid>/environ` | Partial | FR-SEC-13 scopes it read-only, single-repo, short-lived |
| Steal the rig token from the runtime's argv | Accepted | The host already has the memory it protects |
| Tamper with completion content | Partial | Harmless for chat; for the control pane it is FR-SEC-07 and FR-UI-11 |
| **Substitute a smaller model** and bill A100 time for 7B answers | **Partial — see below** | |
| **Misrepresent the hardware** — claim an A100, provide something slower | **Partial — see below** | |
| Open a reverse channel back through the SSH connection | **Mitigated** | The client rejects server-initiated channels and global requests (§15.5.2) |
| Leave weights and KV cache in VRAM for the next tenant | **Open** | Teardown does not scrub GPU memory; the driver's zeroing behaviour is not something LARRI can verify from outside |

**Model substitution is worth dwelling on**, because the defence already exists and was built
for another purpose. The sizing engine (§7) predicts required VRAM, and the telemetry plane
(§17.4) reports actual VRAM in use. A host serving a 7B model while charging for a 70B
produces a visible gap between the two — which is exactly the predicted-versus-actual panel
in §17.7. The same cross-check catches a slower-than-advertised GPU through throughput that
does not match the class.

This detects a lazy adversary, not a determined one: the host also controls `nvidia-smi`, so
it could report figures that match the prediction. It is worth having because the lazy case
is the likely one, and because it costs nothing — the panel exists already.

#### 15.8.2 The Provider — and the limit of host key pinning

| Attack | Status | Note |
|---|---|---|
| MITM the provider control API | **Mitigated** | TLS verification, no disable path (FR-SEC-10) |
| **MITM the SSH session via the proxy** | **Open — structural** | See below |
| Report false instance state or cost | Accepted | The provider is definitionally authoritative about its own inventory |
| Create instances billed to the operator | Accepted | Detected by reconciliation as orphans, not prevented |

Host key pinning (§15.4) defeats a *network* MITM. It does not defeat the provider, and the
reason is worth stating plainly rather than leaving implied: **LARRI learns the host key
fingerprint from the provider's own API.** Pinning a key the adversary supplied proves only
that the same party is on both ends of the connection.

Under Vast's default *proxy* SSH the provider is additionally on the network path. Direct SSH
removes them from the path but not from the trust set, since the address and fingerprint still
come from their API.

There is no clean fix. Out-of-band key distribution has no channel to use; baking a key into
the image means every rig shares it and the host reads it anyway. The honest position is that
**the provider is inside the TCB for transport integrity**, §15.3 now says so, and an operator
who cannot accept that cannot rent from a marketplace.

#### 15.8.3 The Network Path

| Attack | Status |
|---|---|
| Intercept or modify control-plane traffic | **Mitigated** — TLS, verified |
| Intercept or modify the tunnel | **Mitigated** — SSH, pinned key (subject to §15.8.2) |
| MITM during the first-connect TOFU window | Partial — narrow, and stated rather than denied (§15.4) |
| Traffic analysis: infer token counts and message sizes from packet timing | **Accepted** — SSH does not hide volume, and padding would cost throughput for a privacy gain the threat model does not need |

#### 15.8.4 Co-Tenants on the Same Physical Machine

| Attack | Status | Note |
|---|---|---|
| Reach the runtime across containers | **Mitigated** | Loopback bind, and no port mapping (FR-SEC-08, FR-SEC-15) |
| Reach it via the shared public IP | **Mitigated** | Nothing is mapped to reach |
| Recover our VRAM residue after teardown | **Open** | Same gap as §15.8.1's last row, from the other side |
| Escape their container and become the host | Accepted | Reduces to §15.8.1 |

#### 15.8.5 The Operator's Own Machine

| Attack | Status | Note |
|---|---|---|
| Another local user hits `127.0.0.1:<port>/v1` | **Mitigated** | Mandatory per-client token (FR-SEC-09) |
| **Another local user reads the token out of a client config file** | **Open → closed by FR-SEC-26** | Config files are commonly world-readable by default; LARRI must write `0600` and verify |
| Read rig tokens or the ephemeral SSH key from state | **Mitigated** | `0700`/`0600`, enforced at startup (FR-SEC-11) |
| A browser page drives the endpoint | **Mitigated** | Token plus `Host` validation |
| **UI session token leaks via URL** — browser history, referrer | **Open → closed by FR-SEC-27** | Move it to a cookie on first load and strip it from the address bar |
| **Symlink swap on a client config path** to make LARRI write elsewhere | **Open → closed by FR-SEC-28** | Open with `O_NOFOLLOW` and verify the target before writing |
| Malware running as the operator | Accepted | That is the TCB |

#### 15.8.6 The Model's *Content* — not just its tool calls

Tool calls from the served model are already treated as untrusted (§14.4.4). Its **rendered
output** was not, and that is the sharpest gap this review found.

The chat pane renders model output as markdown. Model output comes from an untrusted host.
The pane is served from an origin that can reach the daemon API. **A host that returns
`<img onerror=…>` in a completion is attempting XSS in a page with control-plane reach** —
which would be a far worse outcome than any tool call, because it bypasses the confirmation
gate entirely.

Three controls, and the third is the one that makes the others survivable:

1. Render markdown to a **safe subset**, escaping by default. Never assign untrusted text to
   `innerHTML`.
2. A strict **Content-Security-Policy** on the UI: no inline script, no external origins.
3. **Separate origins for the two panes.** The chat pane and the console pane are served on
   different ports, so script execution in the pane that renders untrusted content cannot
   reach the credentials or daemon session of the pane that holds control-plane access. This
   costs a second listener and buys the difference between "an XSS bug" and "an XSS bug that
   destroys rigs".

#### 15.8.7 Supply Chain

| Attack | Status | Note |
|---|---|---|
| **Malicious weights via pickle deserialisation** | **Open → closed by FR-SEC-29** | `.bin`/PyTorch pickle checkpoints execute arbitrary code on load. Require `safetensors`; refuse pickle formats |
| Typo-squatted or altered model repository | **Partial → FR-SEC-29** | Pin the resolved commit, as §7.1 already does for facts |
| Compromised LARRI image registry | **Open → closed by FR-SEC-30** | Digest pinning proves *which* image, not that it is ours. Sign images and verify signatures |
| Compromised Go dependency | Partial | Checksum database and vendoring; the licence audit (NFR-10) is not a security control |

The pickle row is the one to act on first. It is a known-exploited class, it executes code on
the machine holding your Hugging Face token, and refusing the format costs nothing because
every runtime LARRI supports prefers `safetensors` already.

#### 15.8.8 The Marketplace — where ranking becomes an attack surface

The uncomfortable one, and it is a design-level property rather than a bug.

**LARRI's ranking function optimises for price** (§8: weight 0.40, the largest term). A host
that wants to harvest prompts, tokens, and weights lists *below market*. So the ranking
function, working exactly as designed, steers operators toward the offer an attacker would
place.

Nothing here is fully solvable — a marketplace of anonymous hosts is what it is — but the
weighting should acknowledge it:

- `Reliability` already counters this weakly, since a fresh malicious listing has no history.
- `Criteria.CertifiedOnly` (§15.5.1) is the strong version, and the honest advice for any
  workload the operator would not publish.
- **Price that is anomalously low for the hardware class is a signal, not a bargain**, and
  the ranking should treat a large negative outlier as suspicious rather than optimal.
- `larri offers` already prints the score breakdown (§8), which is what lets an operator see
  *why* something ranked first before renting it.

#### 15.8.9 Summary of Open Items

| # | Gap | Disposition |
|---|---|---|
| 1 | Client config files may be world-readable | Close — FR-SEC-26 |
| 2 | UI session token in the URL | Close — FR-SEC-27 |
| 3 | Symlink swap on client config paths | Close — FR-SEC-28 |
| 4 | Pickle-format weights execute code | Close — FR-SEC-29 |
| 5 | Image digest proves identity, not provenance | Close — FR-SEC-30 |
| 6 | Model output rendered without sanitisation, same origin as control plane | Close — FR-SEC-31 |
| 7 | Anomalously cheap offers are ranked best | Close — FR-SRCH-08 |
| 8 | VRAM residue after teardown | **Accept and document** — not verifiable from outside the host |
| 9 | Provider is inside the TCB for transport integrity | **Accept and document** — no available fix |

---

## 16. Error Taxonomy

The classification drives retry policy, so it is a type, not a convention.

| Class | Meaning | Policy |
|---|---|---|
| `ErrCriteriaUnsatisfiable` | No offer can fit the model. | Reject pre-spend with the VRAM shortfall (NFR-11). |
| `ErrProviderTransient` | Rate limit, 5xx, timeout. | Retry with backoff; if it wraps a mutation, reconcile first. |
| `ErrProviderUnknownOutcome` | Mutation whose result is unknown. | **Never blind-retry.** Reconcile by label, then decide (R-07). |
| `ErrHostFailure` | Host-attributable: sshd, GPU, image pull. | Fall back to the next offer (FR-PROV-05). |
| `ErrModelFailure` | Model- or config-attributable: bad ref, gated, OOM at load. | Do not retry on another host; report. |
| `ErrDestroyUnconfirmed` | Destroy issued, absence not proven. | Retry to deadline, then escalate loudly and keep the rig in the reconciler's list (FR-DEL-04). |
| `ErrWiring` | Client config could not be written or reverted. | Never fatal to the rig; report, keep the backup, continue. |

`ErrProviderUnknownOutcome` is the one that costs real money if mishandled, and it is the
reason mutations are wrapped in a classifier rather than returned raw from the adapters.

---

## 17. Observability

Three signals, one instrumentation API, and a hard rule about which of them may be
believed.

### 17.1 The Telemetry Plane

Observability is the third plane named in §2, and it is subordinate to the other two.

**T1 — telemetry may never affect the control or data plane.** A wedged `nvidia-smi`
session, a full ring buffer, an OTLP endpoint that stopped accepting, a runtime that
exposes no `/metrics` — none of these may fail a rig, block readiness, delay a state
transition, or influence a supervision decision. Every collector runs on its own goroutine
under its own deadline and **drops samples rather than applying backpressure**.

**T2 — the journal remains the authority for money.** Traces are sampled, optional, and may
be exported to a system LARRI does not control. Cost figures, state transitions, and the
audit trail derive from the journal (§11.2), never from telemetry. Where the two disagree,
the journal is right. This is P4, and it is why OTel is added *alongside* the journal rather
than in place of it — a tempting simplification that would trade an authoritative local
record for a lossy remote one.

**T3 — no inference content, ever.** The GenAI semantic conventions permit capturing prompt
and completion content as span events. LARRI does not, and no flag enables it. Operators
run inference on hardware they chose precisely so their prompts stay there; forwarding
those prompts to a telemetry backend would invert the product's purpose. Token *counts*
are telemetry; token *contents* are not.

### 17.2 Structured Logs

`log/slog`, with `rig_id` on every line and `trace_id`/`span_id` whenever a span is in
scope. INFO for state transitions, DEBUG for provider request/response with secrets
redacted, WARN for retries and degraded health, ERROR for anything
billable-and-unconfirmed.

The `Secret` type (§15) makes redaction structural in logs. §17.6 extends the same
guarantee to span attributes, which is a separate code path and therefore a separate risk.

### 17.3 Tracing the Lifecycle

The provisioning sequence (§9) is already a waterfall; a trace makes it a legible one. One
root span per rig, covering the whole billable lifetime:

```
larri.rig                          rig.id, provider, gpu.model, price.usd_per_hour
├─ sizing.plan                     model.ref, quantization, context.len, vram.required
├─ provider.search                 provider, offers.returned, offers.satisfying
├─ rank.select                     offer.score, offer.rank
├─ provider.create        ◄──────── billing starts here
├─ ssh.wait
├─ sizing.verify                   re-run against the hardware actually placed
├─ runtime.bootstrap
│  ├─ image.pull
│  └─ weights.download             bytes.total, bytes.per_second
├─ runtime.launch
├─ tunnel.open                     local.port
├─ runtime.ready                   attempts, round_trip_ms
├─ wire.apply                      clients.configured
│  … rig serves traffic …
└─ teardown
   ├─ wire.revert
   ├─ tunnel.close
   ├─ provider.destroy
   └─ destroy.verify               attempts, confirmed_absent
```

**Every span below `provider.create` carries `larri.cost.usd`** — its duration multiplied by
the rig's hourly price. The waterfall then answers the question the operator actually has,
which is not "what was slow" but "what did I pay for". On a first boot, `weights.download`
is routinely the largest line item; seeing that in a waterfall argues for R-03's mitigations
far better than a paragraph about them does.

Spans survive process death badly, so the root span context is persisted with the rig
(§11.1), and journal entries (§11.2) gain `trace_id` and `span_id` fields. A daemon restart
that adopts a running rig (§12) starts a **new** trace joined to the original by a span
link, rather than pretending the first one continued — the gap is real and the trace should
show it.

### 17.4 Metrics — Four Sources, One Pipeline

| Source | Collected by | Carries | Interval |
|---|---|---|---|
| **Rented host** — GPU utilisation, VRAM used/total, temperature, power, host CPU/RAM/disk/network | `nvidia-smi` polled over one long-lived SSH session, CSV-parsed | `larri.gpu.*`, `system.*` | 1 s |
| **Inference runtime** — KV-cache utilisation, TTFT, inter-token latency, queue depth, throughput, preemptions | Prometheus text scrape of the runtime's `/metrics`, parsed with `expfmt` | `gen_ai.*`, `larri.runtime.*` | 5 s |
| **Local proxy** (§10.1) | in-process; it is already on the data path | request count, tokens in/out, latency | per request |
| **Daemon process** | `contrib/instrumentation/runtime` + `host` | `process.*`, `system.*` | 10 s |

Four sources exist because no one of them suffices, and the gaps are instructive:

- **`nvidia-smi` over SSH is the default for host metrics, not DCGM.** DCGM exporter yields
  better data — SM occupancy, NVLink, ECC — but wants a container, a port, and 50–100 MB of
  RAM on a host whose image LARRI may not control (Q-07 is open). `nvidia-smi` is present on
  any image that could run a runtime at all, needs no install, and is reachable down the SSH
  connection that already exists. DCGM is an opt-in upgrade where the image provides it, and
  it feeds the same metric names, so the console cannot tell which produced a series.
- **The runtime scrape is best-effort by construction.** vLLM exposes a rich Prometheus
  endpoint, llama.cpp a smaller one, Ollama none. The proxy source exists so throughput and
  latency KPIs are available for *every* runtime — which is P2 applied to the console: the
  operator should not be able to identify the runtime behind the endpoint by noticing which
  graphs went blank.
- **Sampling a rented host costs billed time.** Negligible, but not zero, and the reason
  collection multiplexes over the existing SSH connection instead of dialling per poll.

Naming follows OTel semantic conventions where they exist (`system.cpu.utilization`,
`system.memory.usage`, `gen_ai.client.operation.duration`, `gen_ai.client.token.usage`) and
a `larri.*` namespace where they do not. There is **no stable OTel convention for GPU
telemetry**, and the GenAI conventions remain in Development status as of this revision, so
the semconv version is pinned in `internal/telemetry` and treated as a dependency expected
to churn. Domain metrics with no upstream equivalent — `larri.rig.cost.usd`,
`larri.rig.uptime`, `larri.gpu.vram.used`, `larri.gpu.vram.required` — stay in `larri.*`
permanently.

Cardinality discipline: `rig.id`, `provider`, `gpu.model`, and `runtime` are acceptable
attributes, bounded by how many rigs one operator runs. Offer IDs, instance IDs, and model
refs are **not** attached to high-frequency instruments — they belong on spans and in the
journal, where unbounded cardinality is free.

### 17.5 Export — Self-Contained by Default

NFR-06 permits no runtime dependency beyond `ssh`, which rules out *requiring* a collector.
The resolution is that OTel is the instrumentation API everywhere while the export pipeline
is a configuration choice:

| Mode | Reader / exporter | Requires | Default |
|---|---|---|---|
| **Console** | in-memory periodic reader → fixed-capacity ring buffer in the daemon | nothing | **on** |
| **OTLP** | `otlpmetricgrpc` / `otlptracegrpc` → `OTEL_EXPORTER_OTLP_ENDPOINT` | operator's collector | off |
| **Prometheus** | `exporters/prometheus` on a loopback `/metrics` | operator's scraper | off |
| **Disabled** | no SDK registered; the API degrades to a no-op | — | — |

The ring buffer holds one hour at one-second resolution — a few thousand points across a
dozen series, tens of kilobytes — enough to render the console with backfill and small enough
that its footprint never needs discussing.

**It is persisted** (Q-08, resolved), to `~/.local/state/larri/metrics/<rigID>.jsonl`,
downsampled as it ages: 1 s while live, 10 s after an hour, 60 s after a day. The reason is
not that graphs should survive a restart — it is §13.1. A terminated rig retains *why* it
died; retaining the series that led up to it makes the post-mortem complete. "Destroyed on
idle timeout" beside a GPU utilisation trace that flatlined forty minutes earlier is an
explanation. Without the trace it is an assertion.

Retention is therefore aligned with rig retention (FR-DEL-09), not chosen separately: series
age out when their rig's snapshot does, and the journal remains the permanent record of cost
regardless.

Three constraints keep persistence from violating T1:

- **Writes are buffered, batched, and best-effort.** A failed write drops samples and is
  logged; it never propagates, never blocks a collector, and never touches rig state.
- **No CGO, so no SQLite.** §19 requires `CGO_ENABLED=0`, which rules out the obvious
  embedded database. Append-only JSONL with downsampling on rotation is dumb enough to be
  obviously correct and needs no migration story.
- **A corrupt or truncated file is a truncated graph**, never a startup failure. The reader
  stops at the first unparseable line and reports what it has.

Because the OTel API is a no-op until an SDK is registered, instrumenting exhaustively costs
nothing in the disabled case. That is what makes it safe to instrument the money paths as
densely as they deserve.

### 17.6 Secrets in Telemetry

FR-SEC-02 covers logs, TUI output, MCP results, and errors. Span and metric attributes are
a fourth path, covered by two mechanisms because one is a discipline and the other is not:

1. A `SpanProcessor` in `internal/telemetry` scrubs attribute values matching known secret
   shapes on every span before export.
2. The `Secret` type's `String()` returning `"***"` (§15) means a secret formatted into an
   attribute was already redacted at the point of formatting.

Enabling OTLP export sends telemetry off the machine, so the set of attributes that leave is
documented rather than emergent — which is why §17.3 names attributes per span instead of
leaving them to the implementation. Adding an attribute is a review point.

### 17.7 What the Plane Buys

Two panels justify the whole apparatus:

- **Predicted versus actual VRAM.** `SizingPlan.RequiredVRAMBytes` (§7.3) drawn as a
  reference line against live `larri.gpu.vram.used`. Every running rig becomes a live test
  of the sizing math, and systematic over- or under-estimation appears as a visible offset
  rather than as an OOM three weeks later. This is the cheapest available mitigation for
  R-08 and it costs one extra series.
- **Cost against utilisation.** `larri.rig.cost.usd` accruing beside GPU utilisation makes
  an idle rig self-evident. It is the same signal FR-SUP-06's idle timeout acts on, shown to
  the operator rather than only acted upon.

---

## 18. Testing Strategy

**No test issues a real create (NFR-09).** The narrowness of `Provider` and `Runtime` is
what makes this achievable, and is the primary reason those interfaces are as small as they
are.

| Layer | Approach |
|---|---|
| `sizing` | Table-driven unit tests against known models and quantizations, with expected VRAM within a tolerance. The highest-value tests in the repo — this math is what stands between the operator and a paid-for OOM (R-08). |
| `rank` | Golden tests: fixed offer sets, asserted ordering and score breakdown. |
| `provider/*` | Contract tests against recorded HTTP fixtures. One shared conformance suite every adapter must pass, so adapters cannot diverge in behaviour. |
| `state` | Crash-injection: kill between intent and create, mid-snapshot-write, mid-destroy. Assert reconciliation recovers in every case (AC-2.1, AC-2.3). |
| `wire` | Config writers against temp-dir fixtures of real client config files. Assert idempotency and byte-exact revert. |
| `daemon` | Full lifecycle against fake provider + fake runtime, including preemption, degradation, and budget breach. |
| `tools` | One conformance suite over the registry, run through **both** adapters: every tool must present the same schema and produce the same result via MCP and via the chat pane's OpenAI `tools[]` path. Asserts that the consequential set is absent from the chat pane's default advertisement, and that every tool result is redaction-checked before return. |
| `telemetry` | Collectors driven by recorded `nvidia-smi` and `/metrics` payloads, including malformed and truncated ones. Fault injection asserts T1: a stalled or failing collector, a full ring buffer, and an unreachable OTLP endpoint each leave rig state, supervision, and cost accounting untouched (AC-5.3). |
| End-to-end | Build-tagged (`//go:build e2e`) and env-gated. Teardown in `defer` that also runs on panic. Never part of `go test ./...`. |

The fake provider is a first-class component, not test scaffolding: it simulates rate
limits, timeouts-that-actually-succeeded, slow boots, and the full disappearance taxonomy of
§12.1 — stop-without-destroy, stop-then-resume, destroy-that-only-stopped, a `List` that
omits non-running resources, and a provider that becomes unreachable without anything having
changed. Most of the cost safety requirements are only testable because it exists, and the
storage-billing trap in §12.4 is only testable because the fake models a stopped instance as
still costing money.

---

## 19. Build and Release

Module path: **`go.sovrenix.com/larri`** — a vanity import path, so the code host can change
without breaking importers. It requires a page at that path serving a `go-import` meta tag
(`go.sovrenix.com/larri git https://github.com/sovrenix/larri`) for as long as anyone imports
the module; that page is an operational dependency, not a one-off setup step.

```bash
go build ./...
go run ./cmd/larri -- up --help
go test ./...
go test ./internal/sizing -run TestKVCacheFit -v
go test -race ./...
go vet ./... && gofmt -l .
```

Single static binary, `CGO_ENABLED=0`, cross-compiled for linux/amd64, linux/arm64,
darwin/arm64. **No runtime dependencies at all** (NFR-06) — SSH is spoken in-process
(§15.5.2), so there is nothing to install and nothing to be missing on the operator's
machine. Version, commit, and
build date stamped via `-ldflags`. Every source file carries an SPDX
short-form header (`Copyright (C) 2026 Sovrenix Inc.` / `SPDX-License-Identifier:
GPL-3.0-or-later`), and dependency licences are audited for GPL-3.0 compatibility
(NFR-10).

---

## 20. Implementation Milestones

| M | Goal | Delivers | Gate |
|---|---|---|---|
| **M0** | Nothing can spend yet | Module, CI, SPDX header check, licence audit gate, `Secret` type, error taxonomy, **fake provider + fake runtime** | Builds clean; `go vet`, `gofmt`, race, and header checks enforced in CI |
| **M1** | One rig, safely | `config`, `sizing` (live facts + cache), `state` **including the journal**, `provider/vastai`, `runtime/vllm`, `sshx` (in-process, pinned host key, ephemeral key), `wire` (tunnel + proxy + credential boundary), teardown with verified absence and a termination record, CLI `up`/`down`/`status` | AC-1.1 … AC-1.5, AC-2.9, AC-3.4, AC-3.7, AC-3.7, AC-4.6 … AC-4.9, AC-4.12 … AC-4.14, AC-4.16, AC-4.17 |
| **M2** | Cost safety under failure | Reconciliation, orphan sweep, `STOPPED` semantics and resume detection, budget ceilings, idle reclamation, crash injection, provider-unreachable handling | AC-2.1 … AC-2.8 |
| **M3** | Breadth | RunPod adapter, llama.cpp + Ollama runtimes, `offers` / `--dry-run`, image variant selection | AC-3.1 … AC-3.6, AC-4.10, AC-4.11 |
| **M4** | Surfaces | Daemon API + SSE, tool registry, MCP server, TUI, web UI (console + chat panes, separate origins), client config writers, preemption recovery | AC-4.1 … AC-4.5, AC-4.15 |
| **M5** | Observability | OTel SDK wiring, lifecycle traces with cost attribution, host/runtime/proxy collectors, persisted metric store, console graphs, optional OTLP and Prometheus export | AC-5.1 … AC-5.4 |

### 20.1 Why M1 Is Large

M1 looks heavier than "one provider, one runtime" implies, and the reason is that **cost
safety and the security boundary are not features added later — they are constraints on the
first line of code that spends money.**

The journal was originally scheduled for M2. That was wrong: FR-PROV-01 requires intent to
be written *before* the create call, and a milestone that spends money without that produces
exactly the orphaned instance the product exists to prevent. Shipping M1 without it would
mean the first working version is the least safe one.

The same argument covers the rest of M1's security scope. Loopback binding, host key pinning,
the ephemeral per-rig key, and the credential boundary are not hardening applied to a working
tunnel; they are properties of the only tunnel that should ever be written.

### 20.2 Written in M1, Even Though the Feature Lands Later

These cost nothing now and cost a sweep of every package later, so they go in as each package
is written rather than being retrofitted:

| Now | Because |
|---|---|
| OTel instrumentation calls | The API is a no-op until an SDK is registered (§17.5). Retrofitting means touching every package |
| `trace_id` / `span_id` on journal entries | The journal format is durable; adding fields later means migrating written records |
| `Rig.End *Termination` | Teardown must record *why* from the first teardown that exists (§13.1) |
| Per-client token identity | Retrofitting identity onto a shared secret means re-wiring every client |
| Model-name routing in the proxy | A pass-through at N=1, but retrofitting it later reconfigures every client — the churn P3 exists to prevent (§10.3) |
| `Provider.List` returning non-running resources | The one shape that makes `STOPPED` detectable at all (§12.4) |

### 20.3 Sequencing Notes

M0 exists so that the fake provider and fake runtime are available *before* the code that
would otherwise be tested against a paid API. The fake is not test scaffolding added at M2;
it is what makes M1 developable without spending (NFR-09).

M2 precedes breadth because cost safety is the property that makes the tool trustworthy
enough to use daily (NFR-01). M5 is last but not deferred — see §20.2.

**M1 uses a stock image, not a pre-baked one.** Q-07 ratified pre-baked images and §6.5 stands,
but building and publishing a signed image matrix is a separate workstream, and blocking the
first working rig on registry and signing infrastructure would invert the point of M1 — which
is to prove the Provider/Runtime seam holds. M1 therefore exercises the stock-image fallback
path that §6.5 already requires, which has the useful side effect that the fallback is tested
by the milestone that depends on it rather than rotting unused. The image pipeline, digest
pinning, and signature verification (FR-SEC-30) follow as their own milestone, and M3 takes
image variant selection once there are variants to select.

## Appendix A — Worked Example

```console
$ larri up --model Qwen/Qwen3-Coder-30B --quantization q4_K_M --context 32768 \
           --gpu A100 --max-price 1.50

  sizing    Qwen3-Coder-30B @ q4_K_M, 32768 ctx
            weights 19.1 GB · kv-cache 4.2 GB · overhead 2.1 GB → 27.9 GB required
  search    vastai 214 offers · runpod 38 offers → 19 satisfy criteria
  ranked    1. vastai  A6000 48GB $0.81/hr  rel 0.91   ← cheapest that fits
            2. vastai  A100 80GB  $1.29/hr  rel 0.98
            3. runpod  A100 80GB  $1.44/hr  rel 0.99
            excluded: A6000 48GB $0.22/hr  3.7× below class median $0.81
  → select  vastai #7710021 @ $0.81/hr   [confirm? y]

  create    intent journaled · instance 14872213 · label larri:01J9Z…
  boot      image ✓ · weights 19.1 GB ████████████ 100% (4m12s, $0.09)
  launch    vllm serve … --served-model-name qwen3-coder --max-model-len 32768
                         --gpu-memory-utilization 0.86
  tunnel    127.0.0.1:8000 → 14872213:8000
  ready     completion round-trip 412ms ✓
  wire      2 clients configured (backed up)

  ✓ rig 01J9Z… READY   http://127.0.0.1:8000/v1   model: qwen3-coder
    $1.29/hr · elapsed 6m04s · accrued $0.13
    tear down with: larri down

$ larri down
  drain     reason operator-request (cli) · wiring reverted (2 clients) · tunnel closed
  destroy   vastai 14872213 … confirmed absent ✓
  ✓ rig 01J9Z… DESTROYED   ran 1h14m · total $1.59

# …or, when nobody was watching:

  ⚠ rig 01J9Z… DESTROYED   ran 2h14m · total $2.87
    policy: idle-timeout · no operator inference for 31m (window 30m)
    last request 13:51:04 · inspect with: larri status 01J9Z…
```

Note what was excluded rather than merely out-ranked. A $0.22/hr A6000 would have been the
cheapest thing that fits, and it is not selected because it sits 3.7× below its class median
— the shape a host fishing for renters would list (§8.2). The operator can take it anyway by
lowering the floor. What they cannot do is take it by accident, which is why selection prints
its exclusions rather than only its choice.
