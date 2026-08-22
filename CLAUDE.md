# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Status: greenfield

The repo currently contains only `README.md` and a GPL-3.0 `LICENSE` (one commit, `main`).
There is no `go.mod`, no source, and no build yet. Everything below is the agreed design
contract for the code that gets written — treat it as the spec, not as a description of
existing files. Update this file as the real structure lands.

## Authoritative documents

Read these before doing design work. They are the ground truth; this file is the operating
summary of them.

| Document | File | Covers |
|---|---|---|
| **Requirements Specification** (LARRI-REQ-001) | [`docs/LARRI_Requirements_Specification.md`](docs/LARRI_Requirements_Specification.md) | Scope, actors, the rig lifecycle state machine, numbered functional requirements (FR-*), non-functional requirements, acceptance criteria per milestone, risks, open questions. |
| **Design Document** (LARRI-DES-001) | [`docs/LARRI_Design_Document.md`](docs/LARRI_Design_Document.md) | Architecture, package layout, core Go types, Provider/Runtime interfaces, sizing math, ranking function, provisioning sequence, state/reconciliation, teardown protocol, daemon API, error taxonomy, observability and the telemetry plane, testing strategy, milestones. |

When code and these documents disagree, fix one of them deliberately — do not leave the
divergence. **All of Q-01…Q-11 are now resolved** — see §13.1 of the requirements spec,
which records the reasoning and not just the verdict. New questions go there rather than
being assumed away; answer them with the operator.

Decisions worth knowing before touching related code: IDEs are wired via **Continue.dev**
(one file for VS Code and JetBrains) and VS Code BYOK; chat via **LibreChat** (primary),
Open WebUI (conditionally), and AnythingLLM (self-hosted by file, desktop guided) — **not
Cursor**, which proxies through its own backend and so cannot reach a loopback endpoint.
Client writers declare a **writability tier**, decided by *observed reload behaviour*: an app
that snapshots an env var into its own database on first run is tier B no matter how
file-configurable it looks. Probe after writing, always. Images are
**pre-baked and digest-pinned**, selected against the host driver as a search filter. Model
facts are **live-fetched from Hugging Face** and cached by commit. Interruptible offers are
**opt-in**. Budget breach and idle both **destroy**, and both explain themselves. One rig at a
time by configuration; the fixed local port **routes on served-model name** so N>1 needs no
client reconfiguration. Metrics are **persisted** alongside the termination record.

## What LARRI is

**L.A.R.R.I. = Local Agent for Remote Rigging of Inference.**

A local agent that rents someone else's GPU, stands up an inference server on it, points
your local tools at it, and keeps it alive only as long as you want it. One sentence, one
lifecycle:

```
criteria → search offers across providers → rank → provision → bootstrap runtime
        → wait for readiness → publish a stable local endpoint → rewire IDE + chat
        → supervise → destroy on user command
```

Criteria are things like GPU model, VRAM, CPU cores, RAM, disk, region, max $/hr, and the
model to serve. Providers are Vast.ai and RunPod first, more later. Runtimes are llama.cpp,
Ollama, and vLLM.

The user's mental model is a single toggle: `up` gives them a working local endpoint, `down`
guarantees they stop paying. Every design decision below exists to protect that.

## Load-bearing invariants

These are the things that require reading several packages to reconstruct, so they are
written down here instead.

### 1. Two abstractions, and only two

**Provider** (`Search(Criteria) []Offer`, `Create(Offer) Instance`, `Status`, `Destroy`) and
**Runtime** (`Bootstrap(Instance, ModelSpec)`, `Readiness`, `Serve`). Provider-specific
vocabulary — Vast's offers/asks and interruptible bids, RunPod's pods and Secure vs
Community Cloud — is normalized at the provider boundary and must not leak upward. If
core/ranking/wiring code has to branch on which provider it is talking to, the abstraction
is wrong; fix the boundary rather than adding the branch.

Provider APIs churn. Verify request/response shapes against current provider docs before
trusting anything written here or in existing code.

### 2. OpenAI-compatible `/v1` is the contract

llama.cpp (`llama-server`), Ollama, and vLLM all expose an OpenAI-compatible HTTP surface.
That surface — not any runtime's native API — is what the wiring, the chat UI, and the IDE
depend on. Runtimes differ in three places only, and those differences belong inside the
Runtime implementation:

- how model weights are acquired (HF repo pull vs registry pull vs pre-baked image),
- how VRAM fit is computed (quantization, KV-cache size, context length, tensor parallelism),
- what "ready" means (a real completion round-trip, not a TCP connect or a 200 on `/health`).

### 3. The local endpoint is stable; the remote host is not

Local clients must be configured **once**, against a fixed local address (e.g.
`http://127.0.0.1:<port>/v1`). LARRI owns the hop from there to the current instance —
SSH tunnel or local reverse proxy. Never write the ephemeral provider host:port into IDE or
chat config, or every teardown, migration, or spot-preemption forces a config rewrite of
every client. Replacing the instance behind a live local port is the normal case, not an
edge case.

Rewiring local clients means touching files outside this repo (IDE settings, chat config).
Back up before writing, write idempotently, and be able to revert cleanly on `down`.

### 4. State is money

A rented GPU bills by the second whether or not LARRI is running. Consequences that must
hold in code:

- The durable state file (instance id, provider, created-at, price, tunnel port, PIDs) is
  the source of truth, written **before** the create call is issued, not after it returns —
  a crash mid-create must still leave a trail.
- On daemon start, reconcile: list live instances at every configured provider and reconcile
  against local state. Anything running that local state does not know about is an orphan and
  must be surfaced loudly.
- `Destroy` is idempotent and confirmed by re-querying the provider. "The API returned 200"
  is not proof the instance is gone.
- **Stopped is not gone.** A preempted or host-stopped instance is *not* destroyed: on
  Vast.ai the container still exists and storage still bills for every second it does,
  sometimes at a higher rate than while running. Only absence from the provider's inventory
  ends billing — so `List` must enumerate non-running resources, and teardown verifies
  absence, not a changed status field. The state is called `STOPPED`, not `PREEMPTED`,
  because the old name sounded terminal and it is not.
- A stopped interruptible can **resume by itself** when its bid clears. If a replacement was
  provisioned meanwhile, that is two billing instances — detect and surface both.
- **Unreachable is not absent.** A failed provider query resolves nothing. Hold the state,
  keep supervising, conclude nothing.
- Supervision classifies by evidence across six cases (absent / stopped / ssh-down /
  runtime-down / wedged / provider-unreachable), not by a preempted-vs-unhealthy binary, and
  must never silently re-provision at a higher price without the user's consent.
- **Idle reclamation is on by default** (`--idle-timeout 30m --idle-action destroy`). Only
  operator inference counts as activity — LARRI's own health probes must be excluded, or the
  timer resets every interval and never fires. `destroy` is the only reclamation action;
  `stop` keeps paying storage and surrenders the GPU.
- **Every teardown records why, with evidence.** A typed `Termination` — actor (operator /
  policy / provider / fault), reason code, and the facts behind it — resolved at the moment
  of the decision, journalled with the intent, never reconstructed from logs afterwards.
  Terminated rigs are retained so `larri status` can still answer "why is my rig gone" long
  after it went. An automated destruction that can explain itself is safe; one that cannot
  is not, and that is what makes destroy-by-default defensible.
- Show running cost. The user should never have to open a provider dashboard to learn what
  they are spending.

### 5. VRAM math is cross-cutting

`(model, quantization, context length)` → required VRAM is a single computation consumed in
three places: the search filter, the ranking function, and the runtime's launch flags. Keep
it in one package and unit-test it hard. Silently over-committing VRAM is how you pay for an
instance that OOMs on first request.

### 6. Interfaces are clients, not owners

The CLI, the TUI dashboard, the MCP server, and the local web chat UI are **four front-ends
over one daemon API**. No provisioning, ranking, wiring, or teardown logic in any of them.
Adding a capability means adding it to the daemon and exposing it in each surface; if a
feature only works from the CLI, it is in the wrong layer.

- **CLI + daemon** — the core. `larri up --gpu a100 --vram 80 --model <name>`, `larri status`, `larri down`.
- **MCP server** — LARRI's operations as tools, so **external** agents (Claude Code and similar coding/chat agents) can drive the whole lifecycle. Destroy/create are consequential; keep them explicit and confirmable.
- **TUI** — live offers, provisioning progress, health, throughput, accrued cost.
- **Web UI** — two panes. The **chat pane** talks to the stable local `/v1` as an ordinary
  client, and exists so the operator can drive and debug LARRI *by talking to the model the
  rig is serving* — it advertises LARRI's tools and executes the calls the model emits. The
  **console pane** shows live KPIs and graphs (cost, GPU, VRAM, CPU, RAM, throughput) from
  the *daemon API*, not `/v1`.

**One tool registry, two drivers.** `internal/tools` holds the canonical definition of every
tool — schema, handler, consequential flag. `internal/mcpsrv` adapts it for external agents;
`internal/webui` adapts it to OpenAI `tools[]` for the rig's own model. Two adapters, never
two definitions.

**The served model is untrusted input.** It runs on a stranger's hardware (invariant 8), so
a hostile tool call can arrive by host compromise, by prompt injection from pasted content,
or by plain hallucination — indistinguishable on the wire, so all three get the same
treatment. The chat pane's default tool set therefore **cannot spend, destroy, or rewire**;
consequential tools are opt-in and always require operator confirmation. Tool results are
redacted *before being returned to the model*, because they are posted back to the untrusted
host on the next turn — `larri_logs` echoing a token is the concrete way this leaks.

Traffic rule for both panes: **inference flows only over `/v1`; actions flow only through the
tool registry.** Neither pane may invent a bespoke daemon endpoint.

Note the bootstrap paradox: if the rig is degraded or destroyed there is no model to ask,
which is why MCP stays primary for control — Claude Code can debug a rig that is not
serving, and the chat pane structurally cannot.

The web UI is Go-served with **no Node toolchain**: stdlib `html/template`, vanilla JS,
uPlot, `go:embed`. This is a ratified decision (resolves Q-02) — do not reintroduce a
JS build step. React + Vercel AI SDK was evaluated and rejected: `useChat` speaks the AI SDK
UI message stream protocol rather than raw OpenAI SSE, so its intended architecture requires
a Node server route at runtime, which NFR-06 forbids. Wails/Fyne/Gio are excluded by
`CGO_ENABLED=0`. Rich chat is delegated to existing clients wired by the `wire` layer —
making Open WebUI and the IDE work against the rig beats reimplementing them.

The console pane needs a browser-reachable bridge to the unix-socket daemon API. That
listener binds loopback only, requires a per-invocation session token, and validates
`Origin`/`Host` — an unauthenticated localhost admin port is reachable by DNS rebinding.

### 7. Telemetry is a third plane, and it is subordinate

Observability uses the OpenTelemetry API throughout, but three rules constrain it:

- **It may never affect the other two planes.** A wedged `nvidia-smi`, a full ring buffer, a
  dead OTLP endpoint, a runtime with no `/metrics` — none may fail a rig, block readiness,
  delay a transition, or influence supervision. Collectors are goroutine-isolated,
  deadline-bounded, and drop samples rather than apply backpressure.
- **The journal stays authoritative for money.** Cost, transitions, and the audit trail come
  from the journal, never from telemetry. Telemetry is sampled and optional; the journal is
  neither. Do not "simplify" by deriving cost from metrics.
- **No prompt or completion content, ever.** The GenAI semconv permits capturing it; LARRI
  does not, and no flag enables it. Operators run local inference for privacy. Token counts
  are telemetry; token contents are not.

Export is opt-in — OTLP and Prometheus are off by default and the console is fed by an
in-process ring buffer, so the binary keeps working with zero external services (NFR-06).
Since the OTel API is a no-op until an SDK is registered, instrument as each package lands.

Spans below `provider.create` carry `larri.cost.usd`, so a trace waterfall shows where the
money went — usually the weight download.

### 8. Security: the host is the boundary

The full threat model is §15.3–15.7 of the design doc. The parts that are easy to undo by
accident:

- **The runtime binds loopback on the rented host, and it is not configurable.** No flag, no
  config key; a non-loopback bind is rejected at launch. A routable inference port is
  unauthenticated access to hardware you are paying for — on Vast, on a public IP *shared
  with other tenants*. The tunnel forwards to the remote loopback, so this costs nothing.
- **Network isolation is two layers, and no firewall:** request a port mapping for SSH only
  (provider-enforced, needs no in-container privilege), and bind loopback. That leaves sshd as
  the only externally reachable service; everything else rides the tunnel. Host firewalling was
  evaluated and rejected — Vast runs clients in *unprivileged* containers so `NET_ADMIN` is
  not available at all, source-IP allow-listing breaks under Vast's default proxy SSH, and it
  bought little over these two. Neither provider offers a security group or inbound IP
  allow-list; withholding the port mapping *is* the provider-side control, and it beats a host
  firewall because the host cannot undo it and it fails closed.
- **On RunPod, declaring a port publishes it** at `https://[POD_ID]-[PORT].proxy.runpod.net`
  through Cloudflare, with Pod-ID obscurity as its only protection. Declare SSH and nothing
  else; the loopback bind is what makes a mistaken declaration harmless.
- **SSH is spoken in-process** (`golang.org/x/crypto/ssh`), not by driving an `ssh` binary.
  That makes the hardening structural rather than configural: agent forwarding, X11, remote
  forwarding, and password auth are **not implemented**, and `~/.ssh/config` is never read —
  a capability that does not exist cannot be re-enabled by someone's personal config. Agent
  forwarding is the one that matters most: a forwarded agent lets a root-holding host
  authenticate *as the operator* everywhere their key is trusted. The local forward binds via
  `net.Listen` before readiness, so a failed forward is a Go error rather than a tunnel that
  looks healthy and carries nothing. NFR-06 is therefore **no runtime dependencies at all**.
- **Ephemeral ed25519 keypair per rig**, never the operator's long-lived identity. Teardown
  discards it, so revocation is automatic and access cannot outlive the rig.
- **Teardown must never depend on SSH.** Destroy is a provider API call, which is the property
  that makes firewalling safe to attempt: lockout can never create an unkillable billing
  instance.
- Provider direct port-mapping is plaintext on a public port and stays off by default.
- **SSH host key pinning is mandatory**, in a LARRI-owned `known_hosts`. Never emit
  `StrictHostKeyChecking=no` or `UserKnownHostsFile=/dev/null` — they are the usual
  workaround for rented boxes and they disable the only thing standing between you and a
  network MITM. A key that changes mid-rig is compromise, not inconvenience.
- **Two credentials, opposite lifetimes, and the proxy is the boundary between them.** A
  *client token* guards the local listener and is **stable across rigs** (rotating it would
  rewrite every client config on every teardown — the exact churn P3 exists to prevent). A
  *rig token* guards the runtime and is **ephemeral, one per rig**, rotated on every instance
  replacement. The proxy **strips** the incoming `Authorization` and substitutes the rig's —
  header pass-through is the default in most reverse proxies and would ship the token shared
  by all your IDEs to untrusted hardware. Client tokens are per-client, so one can be revoked
  without rewiring the rest and spend can be attributed per tool.
- Don't build anything stronger than a bearer token *inside* the tunnel: the channel is
  already SSH-authenticated to a loopback service, and the only principal in there is the
  host, which has root and would read the runtime's memory rather than its API. The rig token
  is a fail-safe for the day a bind or a port mapping is wrong, not a defence against someone
  already inside.
- **The local `/v1` API key is mandatory, not optional.** Loopback is *not* a per-user
  boundary, and any page in the operator's browser can fire requests at a loopback port —
  and for LARRI a request that fires is a request that spends. Validate `Host` too.
- **DoS here is a financial attack.** Rate limits, concurrency caps, and per-request token
  ceilings are cost controls, not just availability controls.
- **Model output is untrusted *content*, not just untrusted tool calls.** The chat pane renders
  text from a host you do not trust. Safe-subset markdown only, escaped by default, never raw
  `innerHTML`; strict CSP; and **chat and console are served on separate origins**, so XSS in
  the pane rendering untrusted content cannot reach the pane with control-plane access.
- **Refuse pickle-format weights; require `safetensors`.** PyTorch pickle checkpoints execute
  arbitrary code on load, on the machine holding your Hugging Face token. Costs nothing to
  refuse — every supported runtime prefers `safetensors` already.
- **Ranking is an attack surface.** Price is its heaviest term, so a host hunting for renters
  lists below market and our own scoring recommends it. Anomalous underpricing is a signal,
  not a bargain (FR-SRCH-08); `CertifiedOnly` is the strong control.
- **Host key pinning does not exclude the provider** — LARRI learns the fingerprint from the
  provider's own API, so pinning proves only that the same party is on both ends. The provider
  is inside the TCB for transport integrity. There is no clean fix; it is accepted and stated.
- **None of this protects you from the host, which has root.** Prompts, completions, weights,
  and any token in the process environment are visible to whoever owns the machine.
  Confidential computing is the only real answer and is not available on commodity
  marketplace hardware. Send a rented GPU nothing you cannot afford to disclose.

### 9. Secrets

Provider API keys and SSH keys come from the environment or the OS keyring. They are never
committed, never written into state files, and never echoed into logs or TUI output.

## Error messages

Error strings are terse and program-like. Informational output — progress,
status, disclosures, the shortfall message — may be conversational, because it
is read by someone who is not currently stuck.

```
package: subject: problem
```

- **Lowercase, no trailing punctuation.** Errors compose when wrapped; a capital
  or a full stop mid-chain reads wrong: `up: sizing: Unknown quant.: no offers`.
- **State the fact, not the reasoning.** "unknown quantization %q" — not
  "refusing to guess, because a guessed weight size is a confident wrong
  answer". The reasoning belongs in the doc comment, where a reader who wants it
  will look.
- **The class carries the policy; the message carries the fact.** A
  `ClassModelFailure` already means "do not retry on another host", so the
  message says `cuda out of memory loading %s` and nothing about retries.
- **Append a remedy where one exists**, terse: `: expected one of fp16, bf16, …`
  or `: safetensors required`.
- No `because`, `refusing to`, `please`, `you must`, `which means`, `so that`.

This is enforced by `internal/lint`, which walks the AST of every non-test file
and fails the build on a violation. A convention nobody checks decays into a
convention nobody follows.

## Licence, copyright, and file headers

LARRI is a **public open-source project**. Copyright holder: **Sovrenix Inc.** Licence:
**GPL-3.0-or-later**. `LICENSE` holds the full GPL text; `NOTICE` holds the canonical
copyright notice.

**Every source file starts with an SPDX short-form header** — no exceptions, including
generated and test files:

```go
// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later
```

Use the comment syntax of the language (`#` for shell/YAML, `<!-- -->` for standalone HTML).
SPDX short form is used rather than the full GPL boilerplate so headers stay readable; the
identifier refers to the full notice in `NOTICE`. `GPL-3.0-or-later` — not `-only` — matches
the "or (at your option) any later version" wording the FSF recommends.

Dependency licences must be GPL-3.0-compatible (NFR-10). Apache-2.0, MIT, BSD, and ISC are
fine; anything else needs a deliberate decision before it enters `go.mod`.

The mascot is **Larri the lobster** (a lobster leaves one shell for the next and stays the
same lobster — which is invariant 3). Keep it in the README; do not scatter it through the
docs.

## Stack and layout

Go. Single binary, subcommands for the surfaces. Module path **`go.sovrenix.com/larri`** — a
vanity path, so it needs a `go-import` meta tag served at that URL for as long as the module
is importable.

```
cmd/larri/            CLI entrypoint + subcommands (up, down, status, daemon, mcp, ui)
internal/provider/    Provider interface + vastai/, runpod/ implementations
internal/runtime/     Runtime interface + llamacpp/, ollama/, vllm/ implementations
internal/sizing/      VRAM/context/quantization math (invariant 5)
internal/rank/        Offer scoring: fit, price, reliability, region
internal/state/       Durable lifecycle state + reconciliation
internal/wire/        Tunnel/proxy + IDE and chat client configuration
internal/daemon/      Supervisor loop and the API the front-ends consume
internal/telemetry/   OTel setup, exporters, metric collectors, ring buffer
internal/webui/       Embedded chat pane + KPI console (go:embed assets)
```

## Commands

Standard Go toolchain — these apply once `go.mod` exists:

```bash
go build ./...                                    # build
go run ./cmd/larri -- up --help                   # run the CLI
go test ./...                                     # all tests
go test ./internal/sizing -run TestKVCacheFit -v  # a single test
go test -race ./...                               # race detector
go vet ./...
gofmt -l .                                        # must print nothing
```

## Testing against paid, stateful APIs

Tests must never issue a real `create` or leave anything billable behind. Provider clients
are exercised against recorded fixtures; the Provider and Runtime interfaces are what makes
this possible, so keep them narrow enough to fake. Live end-to-end runs against a real
provider are a deliberate, manually invoked path — build-tagged or env-gated, never part of
`go test ./...` — and they tear down in a `defer` that also runs on panic.
