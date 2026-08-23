# L.A.R.R.I.
# Requirements Specification

**Local Agent for Remote Rigging of Inference**

---

| Field | Detail |
|---|---|
| **Document Title** | LARRI — Requirements Specification |
| **Document ID** | LARRI-REQ-001 |
| **Version** | 0.16 — Cheapest-With-Floors Selection |
| **Status** | Draft for Review |
| **Author** | Ram Katru |
| **Date** | 2026-08-21 |
| **Repository** | `sovrenix/larri` |
| **Copyright** | © 2026 Sovrenix Inc. |
| **Licence** | GPL-3.0-or-later |
| **Companion** | [LARRI Design Document](LARRI_Design_Document.md) (LARRI-DES-001) |

---

## Table of Contents

1. [Motivation — The Manual Workflow Being Replaced](#1-motivation--the-manual-workflow-being-replaced)
2. [Executive Summary](#2-executive-summary)
3. [Scope](#3-scope)
4. [Actors](#4-actors)
5. [Glossary](#5-glossary)
6. [The Rig Lifecycle](#6-the-rig-lifecycle)
7. [Functional Requirements](#7-functional-requirements)
8. [Non-Functional Requirements](#8-non-functional-requirements)
9. [Constraints and Assumptions](#9-constraints-and-assumptions)
10. [Acceptance Criteria](#10-acceptance-criteria)
11. [Out of Scope](#11-out-of-scope)
12. [Risks](#12-risks)
13. [Open Questions](#13-open-questions)

---

## 1. Motivation — The Manual Workflow Being Replaced

The workflow LARRI automates already exists as a hand-run playbook — the sequence anyone
follows the first time they stand a real model up on rented hardware:

1. Choose an instance by eyeballing the Vast.ai marketplace (1× A100 80GB, ≥150 GB disk,
   a CUDA/PyTorch image).
2. SSH in, `pip install vllm`, authenticate to Hugging Face if the weights are gated.
3. `vllm serve <model> --host 127.0.0.1 --port 8000 --served-model-name <stable-name>
   --max-model-len 8192 --gpu-memory-utilization 0.55 --api-key <key>`.
4. Reach the port — `ssh -L 8000:127.0.0.1:8000 -p <port> root@ssh4.vast.ai`.
5. Export a pile of environment variables so the local client targets `127.0.0.1:8000`.
6. Curl a health endpoint and a real completion to confirm the backend actually fired.
7. Remember to destroy the instance.

Every step is manual, every step is error-prone, and step 7 is the one that costs money
when it is forgotten. Each of the seven maps to a requirement group below.

**Step 3 is written above as LARRI does it, not as the playbooks do it.** The published
recipes bind the inference server to every interface on someone else's machine and then
expose it through a provider port mapping. That is unauthenticated access to rented
hardware on a public address shared with other tenants, and it is the single most common
way these deployments leak. LARRI binds loopback and reaches it through the tunnel
(FR-SEC-08); the insecure form does not appear in this document, in the codebase, or in
anything LARRI can be configured to emit.

**The product goal is to collapse that playbook into two commands:** `larri up` and
`larri down`.

---

## 2. Executive Summary

LARRI is a locally-run Go agent that rents GPU capacity from a marketplace provider
(Vast.ai, RunPod, others), stands up an OpenAI-compatible inference server on it, exposes
that server at a **stable local address**, points the user's IDE and chat client at it, and
supervises it until the user tears it down.

The user's mental model is a single toggle:

- **`larri up`** — "give me a working local `/v1` endpoint that meets these criteria."
- **`larri down`** — "guarantee I have stopped paying."

Everything in this specification exists to protect that model. The two properties that
matter most are **truthful readiness** (LARRI never reports READY until a real completion
has round-tripped) and **cost safety** (LARRI never loses track of a billable resource).

---

## 3. Scope

### 3.1 In Scope

| Area | Included |
|---|---|
| Providers | Vast.ai, RunPod. Pluggable interface for others. |
| Runtimes | llama.cpp (`llama-server`), Ollama, vLLM. |
| Selection criteria | GPU model, GPU count, VRAM, CPU cores, RAM, disk, region, max price/hr, reliability, interruptible vs on-demand, target model. |
| Lifecycle | Search, rank, provision, bootstrap, verify, wire, supervise, destroy. |
| Local integration | Stable local `/v1` endpoint; automated IDE and chat-client configuration. |
| Surfaces | CLI + daemon (core), MCP server, TUI dashboard, local web chat UI. |
| Cost | Live accrual display, budget ceilings, orphan detection. |

### 3.2 Out of Scope for v1

Multi-node/distributed inference; training or fine-tuning; provider billing/account
management; hosting a public endpoint for third parties; model quality benchmarking;
Kubernetes; Windows-native support (WSL2 acceptable).

---

## 4. Actors

| Actor | Description | Primary need |
|---|---|---|
| **Operator** | The developer at the keyboard. | One command up, one command down, visible cost. |
| **Local Agent** | Claude Code or another MCP-speaking agent. | Drive the lifecycle as tools, with consequential actions gated. |
| **IDE / Chat Client** | VS Code, Claude Code, a browser chat tab. | A `/v1` base URL that does not change. |
| **Provider** | Vast.ai, RunPod. | External, rate-limited, occasionally wrong. |
| **Supervisor** | LARRI's own background loop. | Detect preemption, health loss, and orphans. |

---

## 5. Glossary

| Term | Meaning |
|---|---|
| **Rig** | One managed unit: a provider instance plus the runtime on it, its tunnel, its wiring, and its state. The noun `larri up` creates. |
| **Offer** | A normalized, purchasable unit of capacity returned by a provider search. |
| **Criteria** | The operator's hardware and price constraints for a search. |
| **ModelSpec** | Model identity, quantization, context length, and where weights come from. |
| **Runtime** | The inference server process (llama.cpp / Ollama / vLLM). |
| **Local endpoint** | The fixed `http://127.0.0.1:<port>/v1` that clients are configured against. |
| **Readiness** | A verified completion round-trip, not a TCP connect or an HTTP 200. |
| **Orphan** | A billable provider resource that local state does not account for. |
| **Preemption** | Provider-initiated termination of an interruptible instance. |

---

## 6. The Rig Lifecycle

The canonical state machine. Every functional requirement group below attaches to a
transition in this diagram.

```
                    ┌──────────────────────────────────────────────┐
                    │                                              │
  IDLE ──► SEARCHING ──► SELECTED ──► CREATING ──► PROVISIONED ──► BOOTSTRAPPING
                                          │             │               │
                                          │             │               ▼
                                          │             │            READY ◄──┐
                                          │             │               │     │
                                          │             │               ▼     │
                                          │             │           DEGRADED ─┘
                                          │             │               │
                                          ▼             ▼               ▼
                                       FAILED        STOPPED ──►    DRAINING
                                          │             │               │
                                          └─────────────┴───────────────┘
                                                        ▼
                                                    DESTROYED
                                     (ORPHANED is discovered, never entered)
```

`STOPPED` replaces the earlier `PREEMPTED` state, and the rename is the point rather than
cosmetic. A state names **what is true** — the instance exists and is not running — not why
it happened. Outbidding, a host stopping the machine, and an exhausted account balance all
land here and all bill identically. Why it happened is recorded as the transition reason,
where it belongs.

`STOPPED` also has an edge the diagram cannot draw legibly: it can return to `PROVISIONED`
on its own, without LARRI asking, when an interruptible instance's bid clears again. See
FR-SUP-10.

| State | Meaning | Billable? |
|---|---|---|
| `IDLE` | No rig. | No |
| `SEARCHING` | Querying providers for offers. | No |
| `SELECTED` | An offer chosen, not yet purchased. | No |
| `CREATING` | Create intent durably recorded; call in flight. | **Assume yes** |
| `PROVISIONED` | Instance exists and is reachable; runtime not up. | Yes |
| `BOOTSTRAPPING` | Runtime installing, weights downloading, server starting. | Yes |
| `READY` | Verified completion round-trip; endpoint wired. | Yes |
| `DEGRADED` | Was READY, health checks now failing. | Yes |
| `STOPPED` | Instance exists but is not running — outbid, stopped by the host, or halted for an exhausted balance. The container still exists, so **storage still bills**, on some providers at a higher rate than while running. Neither terminal nor free. | **Yes — storage** |
| `DRAINING` | Teardown in progress. | Yes until confirmed |
| `DESTROYED` | Provider re-queried and confirms absence. | No |
| `FAILED` | Terminal error before READY; may still be billable. | **Assume yes** |
| `ORPHANED` | Reconciliation found a live resource local state does not own. | Yes |

**Rule:** any state whose billability is "assume yes" must be treated as costing money
until the provider is re-queried and proves otherwise.

**Rule:** *stopped is not gone.* Only `DESTROYED` — the provider affirmatively reporting the
resource absent — ends billing. An instance that has stopped, exited, been outbid, or been
switched off by its host still exists and still accrues storage charges. Treating "not
running" as "not costing" is the same error as treating a 200 from a delete endpoint as
proof of deletion, and it is how R-01 happens without anyone forgetting anything.

**Rule:** *unreachable is not absent.* If the provider API cannot be queried, the rig's state
is unknown. Conclude nothing, act on nothing, and keep supervising.

---

## 7. Functional Requirements

Every requirement carries a **Status**. The vocabulary is deliberately small, and the
distinction that matters most is the first one: this project has repeatedly found that code
which passes its unit tests still fails against real hardware, so "implemented" and "shown to
work" are not the same claim.

| Status | Means |
|---|---|
| `live` | Implemented **and** exercised against real hardware or a real provider API |
| `done` | Implemented and unit-tested |
| `part` | Partially implemented — the gap is stated in [PROJECT_STATE.md](PROJECT_STATE.md) |
| `plan` | Specified, not started |

A per-area summary, and the specific gap behind every `part`, is in
[PROJECT_STATE.md](PROJECT_STATE.md).

Priority: **M** = MUST (v1), **S** = SHOULD, **C** = COULD (post-v1).

### 7.1 Criteria and Model Specification (FR-CRIT)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-CRIT-01 | M | `done` | Accept hardware criteria: GPU model (e.g. `A100`, `4090`, `H100`), GPU count, minimum VRAM per GPU and in aggregate, CPU cores, RAM, disk. |
| FR-CRIT-02 | M | `done` | Accept commercial criteria: maximum $/hr, on-demand vs interruptible, minimum provider reliability score, allowed/blocked regions, allowed providers. |
| FR-CRIT-03 | M | `live` | Accept a `ModelSpec`: model identity (Hugging Face repo, Ollama tag, or local path), quantization, and required context length. |
| FR-CRIT-04 | M | `live` | Derive hardware requirements from the `ModelSpec` when the operator has not specified them, so that `larri up --model <name>` alone is a valid invocation. |
| FR-CRIT-05 | S | `done` | Persist **named** criteria profiles (e.g. `--profile coding-rig`) for reuse, saved on explicit request. A profile named `default` may apply to a bare `larri up` **only if the whole profile is echoed** — model, filters and ceilings — before anything is searched or spent. *Amended:* this requirement originally forbade any reuse on a bare invocation, to stop a command reapplying what was typed a fortnight ago. The hazard was silence, not reuse; echoing the applied profile in full removes it, and FR-CFG-08 covers the settings that spend. There is still no implicit last-used slot. |
| FR-CRIT-06 | M | `done` | Reject a request at submit time — before any spend — when the `ModelSpec` cannot fit any offer satisfying the criteria, and explain the shortfall in VRAM terms. |

**Interruptible default (Q-04, resolved): opt-in.** `Criteria.Interruptible` defaults to
`forbid`; bid and spot offers are used only when the operator asks. The original design
leaned toward merely penalising them in ranking, and the discovery that a preempted Vast.ai
instance is *stopped rather than destroyed* — still billing storage, possibly at a higher
rate, and able to resume by itself into a second billing instance (R-13, R-14) — is what
changed the answer. A 0.15 ranking penalty prices the inconvenience of preemption; it does
not price a storage-billing husk the operator has to notice and reap. Interruptible offers
remain fully supported and are often the right choice for a deliberate, watched session,
which is precisely the condition `--interruptible` expresses.

### 7.2 Discovery and Ranking (FR-SRCH)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-SRCH-01 | M | `part` | Query all enabled providers concurrently and normalize results into a single `Offer` type. |
| FR-SRCH-02 | M | `live` | Filter offers by both the operator's criteria and the computed VRAM requirement from §7.1. |
| FR-SRCH-03 | M | `live` | **Select the cheapest offer that meets the criteria and passes the safety floors.** Criteria are a hard filter; fit is a filter, not a scoring term — once the model is known to run, VRAM headroom has no further business competing with price. Every exclusion records a typed reason and evidence, and the selection prints what it rejected alongside what it chose: an operator asking "why not the cheap one" gets the reason, not a score. |
| FR-SRCH-09 | M | `live` | Apply a reliability floor and a price-outlier floor before selecting on price. Both are configurable and both are overridable, but neither may be bypassed by accident: selecting on price alone steers toward exactly what a host harvesting prompts would list. |
| FR-SRCH-10 | M | `done` | Break ties deterministically, so the same market yields the same selection and a choice can be reproduced in a bug report. |
| FR-SRCH-04 | M | `live` | Present ranked candidates with per-offer price, specs, provider, and score before purchase when run interactively. |
| FR-SRCH-05 | S | `live` | Support `--dry-run`: full search, rank, and plan output with zero spend. |
| FR-SRCH-06 | M | `part` | Degrade gracefully when one provider errors or rate-limits — continue with the rest and report the omission explicitly rather than silently narrowing the search. |
| FR-SRCH-07 | C | `plan` | Watch mode: poll until an offer meeting criteria appears below a target price. |
| FR-SRCH-08 | M | `live` | Treat a price anomalously low for the hardware class as a signal rather than a bargain, measured against the **class median** with a robust spread and a minimum sample size — never against the mean, which a long tail of overpriced listings renders meaningless. Exclude and report; never silently drop, and never silently select. |

### 7.3 Provisioning (FR-PROV)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-PROV-01 | M | `live` | Write the create **intent** to durable state before issuing the provider create call. |
| FR-PROV-02 | M | `live` | Provision with an image appropriate to the chosen runtime and inject the operator's SSH public key. |
| FR-PROV-03 | M | `done` | Treat create as possibly-succeeded on timeout or transport error: reconcile against the provider before retrying, never blind-retry. |
| FR-PROV-04 | M | `live` | Enforce an overall provisioning deadline; on expiry, tear down what was created and report `FAILED` with the reason. |
| FR-PROV-05 | S | `live` | On provisioning failure attributable to the host (not the model or config), fall back to the next-ranked offer, up to a bounded number of attempts, and report every attempt and its cost. |
| FR-PROV-06 | M | `done` | Never create a second rig while one is live unless the operator explicitly asks for multiple. |

**Concurrency (Q-05, resolved).** v1 permits **one rig at a time**, enforced as a
configuration limit (`max_concurrent_rigs: 1`) rather than as an architectural assumption.
State, the daemon API, supervision, and reconciliation are all keyed by rig from the start.
The one design decision that cannot be deferred is how clients address multiple rigs behind a
single fixed port (FR-WIRE-09), because retrofitting that would require reconfiguring every
client — the thing P3 exists to prevent.

### 7.4 Runtime Bootstrap (FR-RT)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-RT-01 | M | `live` | Support llama.cpp, Ollama, and vLLM as interchangeable runtimes behind one interface. |
| FR-RT-02 | M | `live` | Select the runtime automatically from the `ModelSpec` when unspecified (e.g. GGUF → llama.cpp; safetensors + large VRAM → vLLM), and allow explicit override. |
| FR-RT-03 | M | `live` | Acquire weights on the remote host, supporting gated repositories via an operator-supplied token that is never persisted to disk on the remote. |
| FR-RT-04 | M | `live` | Launch the runtime with an OpenAI-compatible HTTP surface and a **stable served-model name** independent of the upstream repo path. |
| FR-RT-05 | M | `live` | Compute and apply runtime launch parameters from the sizing engine: context length, GPU memory utilization, tensor-parallel degree, offload layers. |
| FR-RT-06 | M | `live` | Stream bootstrap progress (image pull, weight download percentage, server start) to the operator; a multi-GB download must not look like a hang. |
| FR-RT-07 | M | `live` | Declare readiness only after a real completion request returns a valid response. TCP reachability and `/health` are necessary but not sufficient. |
| FR-RT-08 | S | `live` | Surface remote runtime logs on demand for diagnosis without requiring the operator to SSH in manually. |
| FR-RT-09 | S | `done` | Launch the runtime with tool calling enabled when the model supports it, selecting the runtime-specific parser automatically. Tool calling is a launch-time property; a runtime started without it accepts `tools[]` and silently answers in prose. |
| FR-RT-10 | S | `part` | When the operator requires a control-capable rig and tool-calling support cannot be established for the model, reject before the create call rather than after paying to boot. |
| FR-RT-11 | M | `plan` | Use project-maintained, pre-baked runtime images pinned by content digest rather than composing stock images at boot. Select the image variant against the host's reported driver/CUDA version as a **search filter**, so an incompatible host is excluded before it is rented, not discovered after. Fall back to a stock image with a warning when no compatible variant exists. |
| FR-RT-12 | M | `live` | Resolve model facts by live fetch of the model's `config.json` from Hugging Face, cached by resolved commit; operator override wins, and unresolvable facts are a hard error before spend, never a guess. Gated repositories exercise the Hugging Face token during sizing, so a bad token fails before anything is billed. |
| FR-RT-13 | M | `live` | End a readiness wait as soon as the runtime process is found **absent**, rather than waiting out a stall timeout sized for a model loading in silence. Consult process liveness only when neither log growth nor hardware activity indicates work, and only after the runtime has produced output: evidence of work must outrank absence of a pid, since a runtime may fork, re-exec, or run under an unmatched name. The failure must carry the runtime's own log tail. |
| FR-RT-14 | M | `done` | Obtain the runtime's log location **from the runtime**, never by assuming one engine's path. A daemon that watches the wrong file sees no output and kills a working host on the cold-start limit. |
| FR-RT-15 | M | `live` | Measure every bring-up deadline as time since the last observed **change**, not as elapsed time since a phase began. Fixed deadlines punish slow hardware for being slow, and the cheap hardware LARRI exists to make usable is the slow hardware. A host that has stopped trying stops changing, so stall-based limits still catch it. |

### 7.5 Endpoint Wiring (FR-WIRE)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-WIRE-01 | M | `live` | Expose the remote runtime at a **fixed local address** (`http://127.0.0.1:<port>/v1`) that is stable across rig replacement. |
| FR-WIRE-02 | M | `live` | Support SSH local port-forwarding as the default transport, and direct provider host/port mapping as an alternative. |
| FR-WIRE-03 | M | `plan` | Re-establish the tunnel automatically on transport failure without changing the local port or requiring client reconfiguration. |
| FR-WIRE-04 | M | `plan` | Configure local clients (IDE and chat) to target the local endpoint, writing config idempotently and backing up any file before modification. |
| FR-WIRE-05 | M | `plan` | Revert client configuration to its pre-`up` state on `down`, so a torn-down rig never leaves an IDE pointed at a dead endpoint. |
| FR-WIRE-06 | M | `plan` | Never write the ephemeral provider host or port into client configuration. |
| FR-WIRE-07 | S | `plan` | Support swapping the instance behind a live local port (migration, preemption recovery) with no client-visible change beyond a brief unavailability. |
| FR-WIRE-08 | S | `live` | Require an API key on the local endpoint and refuse to bind to anything other than loopback unless explicitly overridden. |
| FR-WIRE-09 | M | `part` | Route the fixed local port by **served-model name**, so that multiple rigs are reachable through one endpoint clients were configured against once. With a single rig this is a pass-through; the requirement exists so that adding a rig later adds a model entry rather than a reconfiguration. |
| FR-WIRE-10 | M | `plan` | Declare a **writability tier** per client: (A) plain-text config, written idempotently with backup and byte-exact revert; (B) app-owned datastore or live process state, never written directly — use the application's own API or demote; (C) guided manual configuration, with the exact values presented. A writer that cannot promise byte-exact revert must not claim tier A. |
| FR-WIRE-11 | M | `plan` | Wire, in v1 — IDEs: **Continue.dev** for VS Code and JetBrains (one `~/.continue/config.yaml` serves both) and **VS Code native Copilot Chat BYOK**. Chat: **LibreChat** as the primary target (`librechat.yaml`, `endpoints.custom[]`), **Open WebUI** (tier A only where persistent config is disabled, otherwise via its admin API), and **AnythingLLM** — self-hosted by file, desktop guided, never by writing its database. |
| FR-WIRE-12 | M | `plan` | Do not integrate clients that require the endpoint to be reachable from a third-party backend. Cursor routes model traffic through its own servers, so a loopback endpoint is unreachable and support would require publicly exposing the rig — contradicting FR-SEC-03 and the out-of-scope exclusion of public endpoints. |
| FR-WIRE-13 | M | `plan` | Determine the tier from **observed reload behaviour**, not from the presence of a config mechanism. An application that reads an environment variable once and snapshots it into its own datastore is tier B despite appearing file-configurable — a writer that assumes otherwise succeeds on a fresh install and silently does nothing thereafter. |
| FR-WIRE-14 | M | `plan` | Verify wiring by probing after writing, in every tier. Configuration written correctly but not yet loaded — an application that reads its config only at startup — must be reported as needing a restart, never as success. |

### 7.6 Supervision (FR-SUP)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-SUP-01 | M | `done` | Health-check the rig on a configurable interval using a real inference call, not a liveness ping. |
| FR-SUP-02 | M | `done` | Classify a rig that stops serving by **evidence, not inference**, across at least: absent (provider reports not found), stopped (exists, not running, still billing storage), reachable-but-runtime-down, reachable-but-wedged, and provider-unreachable (state unknown). Each has a distinct correct response; collapsing them loses money or abandons a recoverable rig. |
| FR-SUP-03 | M | `part` | Never re-provision automatically at a higher price than the original without explicit operator consent. |
| FR-SUP-04 | M | `live` | Track and display accrued cost for the current rig and elapsed runtime. |
| FR-SUP-05 | M | `part` | Support a budget ceiling (absolute $ or duration), per rig and globally across all rigs, defaulting to **destroy** on breach after a warning with usable lead time. The ceiling must count storage accrued by `STOPPED` rigs, not GPU time alone. Breach records why, with evidence (FR-DEL-08). |
| FR-SUP-06 | M | `done` | Support an idle timeout — no operator inference for N minutes triggers the configured action, which defaults to `destroy`. Configurable per rig and by default (`--idle-timeout 30m` with `--idle-action` of `destroy` or `warn`). Reclamation records why it fired (FR-DEL-08), because a rig that disappears while the operator is away is the case where an unexplained destruction is least acceptable. |
| FR-SUP-07 | M | `live` | Continue to hold the rig if the supervisor itself crashes; a dead daemon must never imply a destroyed instance, and restart must recover ownership from state. |
| FR-SUP-13 | M | `live` | On restart, rebuild the data plane for a rig still running at the provider: install a **newly minted** SSH key on the running instance through the provider API, reconnect, re-attach to the running server, and republish the endpoint **on the same local port** so wired clients recover without reconfiguration. The lost key must be superseded, never retrieved from storage (FR-STATE-05). |
| FR-SUP-14 | M | `live` | Recover the rig's API credential from the running server rather than by relaunching it. Relaunching evicts resident weights and pays the model-load cost again to solve a bookkeeping problem. A server found running without a LARRI-issued credential must be **refused**, not adopted: tunnelling to an unauthenticated endpoint and reporting the rig recovered is worse than failing to recover. |
| FR-SUP-15 | M | `done` | On adoption, verify the host key against the recorded fingerprint and **refuse on mismatch**, classified as a security failure rather than a host failure. Re-pinning is correct only while a host is still settling during boot; after a rig has served, a changed key means the endpoint leads elsewhere, and falling back to another machine would discard the only evidence of it. |
| FR-SUP-16 | M | `done` | Report a rig that cannot be reconnected as **destroyable but not reconnectable**, naming its ongoing hourly cost. A failed resume must never read as a command that changed nothing. |
| FR-SUP-17 | M | `live` | Arm the **host** to stop *serving* when LARRI stops checking in, so an abandoned rig stops answering prompts even after the local process dies. Halting the container is attempted but is **not established to end billing** on Vast (§12.4.1); nothing may describe it as a teardown. It must act on evidence of idleness — GPU, connections to the runtime, log growth, network — and not on elapsed time alone: a missed heartbeat means the operator is gone, not that a weight download or a generation in flight is finished. The host deadline must always exceed the local idle timeout so the better-informed supervisor acts first. |
| FR-SUP-18 | M | `live` | **Never place a provider credential on a rented host.** The host may only stop the runtime; a marketplace container cannot end its own billing — `CAP_SYS_BOOT` is not in its capability bound and signalling PID 1 achieves nothing (measured, §12.4.1). No surface may describe the watchdog as a teardown. Ending the bill is a provider call from the operator's machine (FR-SEC-18), and `larri orphans` is the remedy for a rig orphaned by a crash. |
| FR-SUP-08 | M | `done` | Count only **operator-attributable inference** as activity for the idle timer. LARRI's own health probes (FR-SUP-01) must be excluded, or the timer resets every interval and never fires. Model listings and other non-inference endpoints do not count; a request still streaming does. |
| FR-SUP-09 | M | `part` | Warn before acting on an idle or budget deadline, on every surface, with enough lead time to cancel or extend. Reclaiming a rig the operator was about to use must be preventable, not merely explicable afterwards. |
| FR-SUP-10 | M | `part` | Treat a `STOPPED` rig as billable and unresolved: continue to supervise it, surface it in every surface, and either destroy it or — on explicit operator choice — wait for resume. A stopped interruptible instance may resume by itself; if LARRI has provisioned a replacement, that resume produces two billing instances and must be detected and surfaced. |
| FR-SUP-11 | M | `done` | Never conclude a rig is gone from a failure to reach the provider. Provider-unreachable is an unknown state, and unknown states are held, not resolved. |
| FR-SUP-12 | S | `plan` | Detect a host whose GPU has become unavailable while the instance still runs — a distinct failure from a crashed runtime, and one that no amount of restarting in place will fix. |

### 7.7 Teardown and Cost Safety (FR-DEL)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-DEL-01 | M | `part` | `larri down` destroys the rig, reverts client wiring, closes the tunnel, and reports total cost. |
| FR-DEL-02 | M | `done` | Teardown is idempotent — safe to invoke repeatedly and from any state including `FAILED` and `ORPHANED`. |
| FR-DEL-03 | M | `live` | Confirm destruction by re-querying the provider. A successful API response is not proof of deletion, and neither is a stopped, exited, or paused instance — **only absence from the provider's inventory ends billing**. The reconciliation listing must therefore include non-running resources; a `List` that returns only running instances would report a storage-billing container as destroyed. |
| FR-DEL-04 | M | `done` | On unconfirmed destruction, retry with backoff and escalate to a loud, persistent warning naming the provider, instance ID, and hourly rate. |
| FR-DEL-05 | M | `done` | On daemon start, list live resources at every configured provider and reconcile against local state; report every orphan. |
| FR-DEL-06 | M | `live` | Provide `larri orphans` to list and destroy unaccounted-for resources across all configured providers. |
| FR-DEL-07 | S | `part` | Offer a "panic" operation that destroys every LARRI-created resource across all providers with a single confirmation. |
| FR-DEL-08 | M | `live` | Record a **typed termination reason** for every rig that stops existing, resolved at the moment of the decision and journalled with the teardown intent — never reconstructed afterwards. It must carry the deciding actor (operator, configured policy, provider, or LARRI fault), the reason code, and the evidence behind it. A reason without evidence is not an explanation. |
| FR-DEL-09 | M | `done` | Retain terminated rigs so the operator can inspect why one ended, long after it ended. `larri status` shows the reason for recent rigs and `larri status --all` the full retained set; snapshot retention is bounded by count and age, while the append-only journal remains the permanent record. |
| FR-DEL-10 | M | `part` | Surface the termination reason in every surface — CLI status and the exit output of `larri down`, the event stream, the TUI, the console pane, and the `larri_status` tool result — subject to FR-SEC-06 redaction on the path that returns to the served model. |
| FR-DEL-11 | M | `done` | **Refuse to provision while a rig is already billing**, naming that rig, its hourly cost, and both ways out (`larri resume` to reconnect, `larri down` to destroy). The check reads local state rather than the provider, so an unreachable provider can never become a reason to spend. It refuses rather than warns: a warning printed above a bring-up that proceeds anyway is read after the money is gone. |

### 7.8 State (FR-STATE)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-STATE-01 | M | `live` | Persist rig state durably: provider, instance ID, offer terms, hourly price, created-at, model, runtime, ports, PIDs, and current lifecycle state. |
| FR-STATE-02 | M | `done` | Write state atomically; a crash mid-write must leave the previous valid state intact. |
| FR-STATE-03 | M | `live` | Record state transitions as an append-only history for cost accounting and post-mortems. |
| FR-STATE-04 | M | `live` | Tag every provider-side resource with a LARRI marker so orphans are attributable to LARRI and distinguishable from the operator's manually created instances. |
| FR-STATE-05 | M | `live` | Never persist API keys, SSH private keys, or Hugging Face tokens in state files. |

### 7.9 Surfaces (FR-UI)

All four surfaces are clients of one daemon API. No lifecycle logic lives in a surface.

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-UI-01 | M | `part` | **CLI** — `up`, `down`, `status`, `logs`, `offers`, `orphans`, `daemon`. Scriptable, with machine-readable `--json` output. |
| FR-UI-02 | M | `plan` | **Daemon** — a long-lived local process owning all state and supervision, exposing an API over a loopback socket. |
| FR-UI-03 | M | `live` | **MCP server** — expose the lifecycle as tools so Claude Code and other agents can drive it. Consequential operations (create, destroy) must be explicitly confirmable and must report cost implications in their results. |
| FR-UI-04 | S | `done` | **TUI** — live dashboard: candidate offers, provisioning progress, health, throughput, accrued cost. |
| FR-UI-05 | S | `plan` | **Web chat pane** — a local browser front-end that talks to the stable local `/v1` as an ordinary client. Minimal as a chat client; its purpose is FR-UI-09, not message rendering. Wiring full-featured clients (FR-WIRE) remains the higher-value path for chat itself. |
| FR-UI-06 | M | `part` | Every surface reflects the same state within one health-check interval; no surface may hold private lifecycle state. |
| FR-UI-07 | S | `plan` | **Web console pane** — live KPIs and graphs for a running rig: accrued and projected cost, GPU utilisation, VRAM used against the sizing plan's requirement, host CPU/RAM/disk/network, and inference throughput and latency. Consumes the daemon API, not `/v1`. |
| FR-UI-08 | M | `plan` | The browser-facing listener binds loopback only, requires a per-invocation session token on every daemon-API request, and validates `Origin` and `Host`. Serving a browser surface must not weaken the access control the unix socket provided. |
| FR-UI-09 | S | `plan` | **Chat-driven control** — the chat pane advertises LARRI's tools to the served model and executes the calls it emits, so the operator can inspect and debug the rig by talking to it. |
| FR-UI-10 | M | `done` | Expose LARRI's operations through **one canonical tool registry**, adapted per driver: MCP for external agents (Claude Code and similar), OpenAI `tools[]` for the served model. One definition, never two — a tool must not mean different things to different drivers. |
| FR-UI-11 | M | `part` | The chat pane advertises only tools that cannot spend, destroy, or rewire. Consequential tools are excluded by default; where enabled, each call requires explicit operator confirmation stating the operation and its cost implication, and auto-execution is never permitted. |
| FR-UI-12 | M | `plan` | Bound the chat pane's tool loop: maximum call depth per turn and maximum result size. A model emitting tool calls without terminating must exhaust a budget, not the daemon. |
| FR-UI-13 | S | `plan` | When no rig is serving, the chat pane states that plainly. The console pane and the MCP surface must remain fully functional, since they do not depend on the rig. |

### 7.10 Secrets and Security (FR-SEC)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-SEC-01 | M | `part` | Read provider API keys from environment or OS keyring. Never from the repository, never written to state. |
| FR-SEC-02 | M | `done` | Redact secrets in all logs, TUI output, MCP tool results, and error messages. |
| FR-SEC-03 | M | `live` | Default the local endpoint to loopback-only binding. |
| FR-SEC-04 | M | `live` | Verify the SSH host key on first connect and pin it for the rig's lifetime, in a LARRI-owned `known_hosts` separate from the operator's. Never emit `StrictHostKeyChecking=no` or `UserKnownHostsFile=/dev/null`. A host key that changes mid-rig is treated as compromise, not inconvenience: do not reconnect, mark the rig `DEGRADED`, and tell the operator. |
| FR-SEC-05 | M | `live` | Treat the rented host as untrusted: it is a third party's hardware. Send only what the workload requires, and document that inference traffic leaves the local machine. |
| FR-SEC-06 | M | `plan` | Redact and minimise tool results before returning them to the served model. A tool result is appended to the conversation and posted to the untrusted host on the next turn; `larri_logs` output in particular may echo a token that must not travel back to the host it was withheld from. |
| FR-SEC-07 | M | `plan` | Treat tool calls originating from the served model as untrusted input. A compromised host, an injected instruction in pasted content, and an ordinary hallucination are indistinguishable on the wire, and all three are handled by FR-UI-11 rather than by attempting to tell them apart. |
| FR-SEC-08 | M | `live` | Bind the runtime to loopback **on the rented host**, never to all interfaces, and run it with its own API key. The bind address is computed by the runtime implementation and is **not configurable**: no flag, no config key, and a non-loopback value is rejected at launch rather than warned about. An inference server on a routable port is unauthenticated access to hardware the operator is paying for — on shared-IP providers, on an address other tenants also answer on. |
| FR-SEC-09 | M | `live` | Require an API key on the local `/v1` listener and validate the `Host` header. Loopback is not a per-user boundary, and a page in the operator's browser can issue requests at loopback ports — for LARRI, a request that fires is a request that spends. |
| FR-SEC-10 | M | `done` | Verify TLS certificates on every provider API call, with no option to disable. Send API keys in headers, never in query strings. |
| FR-SEC-11 | M | `plan` | Keep the daemon socket at `0600` inside a `0700` directory and refuse to start on wider permissions rather than silently correcting them. |
| FR-SEC-12 | S | `done` | Default provider direct port-mapping to off. It is plaintext on a routable port; where an operator enables it deliberately, record the choice and treat the runtime API key as load-bearing rather than defence in depth. |
| FR-SEC-13 | S | `part` | Scope credentials sent to a rented host to least privilege — read-only, repository-scoped, short-lived Hugging Face tokens, never reused across rigs. Provider API keys must never reach a host at all. |
| FR-SEC-14 | M | `plan` | Rate-limit and cap concurrency on the local endpoint, and cap per-request token counts. For LARRI, denial of service is a financial attack; the endpoint's throughput ceiling is a spend ceiling. |
| FR-SEC-15 | M | `done` | Request a provider port mapping for SSH only. A container port that was never mapped is unreachable regardless of what listens on it, which makes this the primary network control — provider-enforced, needing no privilege inside the instance and immune to a mistake in a bootstrap script. |
| FR-SEC-16 | M | `live` | Carry all traffic to the rented host over SSH, spoken **in-process**. Agent forwarding, X11 forwarding, remote forwarding, and password authentication are not implemented rather than disabled by option, and the operator's `~/.ssh/config` is never read — a control that cannot be switched on cannot be switched on by someone else's configuration file. Bind the local forward before declaring readiness, so a failed forward cannot masquerade as a healthy tunnel. |
| FR-SEC-17 | M | `live` | Authenticate to each rig with an **ephemeral keypair generated for that rig**, never the operator's long-lived identity. Discard it at teardown, so revocation is automatic and access cannot outlive the rig. |
| FR-SEC-18 | M | `live` | Never make teardown depend on SSH. Destruction is a provider API call, so a host that has become unreachable — including by LARRI's own firewall rules — can still be destroyed, verified absent, and costed. |
| FR-SEC-19 | M | `done` | Never declare a port that the provider would publish. On RunPod a declared HTTP port is automatically served through a public proxy protected only by Pod-ID obscurity; declaring the runtime port there would publish an inference endpoint to the internet. |
| FR-SEC-20 | S | `live` | Prefer a provider-proxied SSH path that requires no public port at all, so a rig can run with zero port mappings. Determine empirically whether that path carries port forwarding and record the result, falling back to a mapped SSH port where it does not. |
| FR-SEC-21 | M | `live` | Authenticate inference requests at **both** ends of the tunnel with distinct LARRI-generated credentials: a client token guarding the local listener and a rig token guarding the runtime. Neither is operator-supplied. The rig token is regenerated per rig and on every instance replacement; the client token is stable across rigs, because rotating it would rewrite every client config on every teardown and defeat the stable-endpoint guarantee. |
| FR-SEC-22 | M | `live` | Strip the client's `Authorization` header at the proxy and substitute the rig's. A client credential must never reach a rented host, and a rig credential must never be visible to a client. Forwarding headers unchanged — the default behaviour of most reverse proxies — would send the token shared by every wired client to untrusted hardware. |
| FR-SEC-23 | S | `done` | Issue a distinct token per wired client rather than one shared secret, so that a single client can be revoked without rewiring the others, a leaked config burns one credential, and inference cost can be attributed per client in the console. |
| FR-SEC-24 | M | `plan` | Never expose an inference credential to browser JavaScript. The chat pane's requests are authenticated server-side by the UI listener's proxy. |
| FR-SEC-25 | M | `done` | Generate credentials as high-entropy random values, compare them in constant time, type them so redaction is structural, keep them out of the journal, and remove them from client configuration on revert. |
| FR-SEC-26 | M | `done` | Write any file containing a credential with `0600` permissions and verify the result. Client configuration files are commonly world-readable by default, which on a shared machine discloses the token to every local account. |
| FR-SEC-27 | M | `plan` | Do not leave the web UI session token in the address bar. Exchange it for a cookie on first load and strip it from the URL, since URLs reach browser history and referrer headers. |
| FR-SEC-28 | M | `plan` | Open client configuration files with symlink-following disabled and verify the target before writing. A path LARRI writes on the operator's behalf must not be redirectable by anything that can create a symlink. |
| FR-SEC-29 | M | `done` | Refuse model weight formats that execute code on load. Require `safetensors`; reject PyTorch pickle checkpoints. Pin the resolved repository commit so the artefact verified is the artefact loaded. |
| FR-SEC-30 | S | `plan` | Sign published runtime images and verify the signature before use. A content digest proves which image was pulled, not that the project produced it. |
| FR-SEC-31 | M | `plan` | Treat model output as untrusted **content**, not only as untrusted tool calls. Render it as a safe markdown subset, escaped by default and never as raw HTML; serve the UI under a strict Content-Security-Policy; and serve the chat pane and the console pane on **separate origins**, so script execution in the pane that renders untrusted content cannot reach the pane that holds control-plane access. |

### 7.11 Configuration (FR-CFG)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-CFG-01 | M | `done` | Operate fully with no configuration file. Values resolve flags → named profile → config file → built-in defaults, and every layer is optional. Configuration is an optimisation over defaults, never a prerequisite (FR-CRIT-04). |
| FR-CFG-02 | M | `done` | **Never block on interactive input unless interactivity has been detected.** Prompt only when stdin and stderr are both terminals, no non-interactive flag or environment variable is set, and the process is not the daemon, the MCP server, or a machine-readable invocation. `larri_up` is an MCP tool, so a prompt on that path is a hang an agent cannot answer. |
| FR-CFG-03 | M | `done` | Print the destructive defaults — idle reclamation and budget action — on any run that creates a configuration, **whether or not the run is interactive**. A default that destroys and was never mentioned is a trap regardless of how well it is reasoned. |
| FR-CFG-04 | M | `done` | Create configuration in core, not in a surface. A surface may drive configuration creation; it may not own it (P5). |
| FR-CFG-05 | S | `part` | Report what was assumed whenever defaults were used in place of configuration, so a non-interactive run is auditable after the fact. |
| FR-CFG-06 | S | `done` | Allow the TUI to explore offers and save the criteria converged upon as a named profile, via the daemon API rather than by writing configuration directly. |
| FR-CFG-07 | M | `plan` | Validate resolvable provider credentials at configuration time without spending. A key that cannot authenticate must surface before it is needed, not during provisioning. |
| FR-CFG-08 | M | `done` | Disclose every **spending or destructive** setting that came from a file rather than a flag, on each run that uses it. FR-CFG-03 covers creation; this covers use, because both directions of a stale limit are harmful: a low ceiling fails as "no offer satisfies the criteria" — which reads as a market problem — and a high one silently removes a guard the operator believes is active. |
| FR-CFG-09 | M | `done` | Create configuration **without prompting**. First run may write a defaults file and must say that it did; it must never open a form. A surface that blocked here would hang every non-interactive caller, and the MCP server worst of all, where a terminal prompt yields a protocol stream that never speaks again. |
| FR-CFG-10 | S | `done` | Offer criteria editing in its own command rather than on the spending path. `larri up` must not interview an operator who asked to rent hardware, and an interrupted form must not leave them wondering what was written. |

### 7.12 Observability (FR-OBS)

| ID | Pri | Status | Requirement |
|---|---|---|---|
| FR-OBS-01 | M | `plan` | Instrument the daemon with the OpenTelemetry API: traces for the rig lifecycle, metrics for cost, host, runtime, and proxy signals. |
| FR-OBS-02 | M | `plan` | Function fully with no telemetry backend configured. Export to OTLP or Prometheus is opt-in and off by default; with no SDK registered, instrumentation is a no-op. |
| FR-OBS-03 | M | `plan` | Telemetry collection, buffering, and export must never fail a rig, block readiness, delay a state transition, or influence a supervision decision. Collectors drop samples rather than apply backpressure. |
| FR-OBS-04 | M | `done` | Cost, state transitions, and the audit trail derive from the journal, never from telemetry. Telemetry is sampled and optional; the journal is neither. |
| FR-OBS-05 | M | `plan` | Never emit prompt or completion content in any telemetry signal. No configuration option enables it. Token counts are permitted; token contents are not. |
| FR-OBS-06 | M | `plan` | Attribute every provisioning span below the create call with the cost accrued during it, so a trace shows where the money went. |
| FR-OBS-07 | S | `plan` | Collect GPU utilisation, VRAM, temperature, power, and host CPU/RAM/disk/network from the rented host without requiring anything a stock CUDA image does not already provide. |
| FR-OBS-08 | S | `plan` | Collect inference throughput, latency, queue depth, and KV-cache utilisation for every runtime, including runtimes that expose no metrics endpoint of their own. |
| FR-OBS-09 | M | `plan` | Redact secrets in span and metric attributes by the same structural mechanism used for logs (FR-SEC-02). |
| FR-OBS-10 | M | `plan` | Persist collected metrics across daemon restarts, downsampled as they age, retained in step with terminated-rig retention (FR-DEL-09) so a post-mortem shows both why a rig ended and the series leading up to it. Persistence is best-effort and subordinate (FR-OBS-03): a failed or corrupt write yields a truncated graph, never a startup failure or a state change. |

---

## 8. Non-Functional Requirements

| ID | Category | Status | Requirement |
|---|---|---|---|
| NFR-01 | Cost safety | `live` | No code path may leave a billable resource unrecorded. This is the highest-priority non-functional property; when it conflicts with any other, it wins. |
| NFR-02 | Reliability | `done` | Every provider call is idempotent or reconcilable. Retries must never risk double-provisioning. |
| NFR-03 | Latency | `live` | Search across providers returns within 10 s. `larri status` returns within 200 ms from cached state. |
| NFR-04 | Latency | `live` | `larri down` confirms destruction within 60 s under normal provider behaviour. |
| NFR-05 | Truthfulness | `live` | Progress and readiness reporting reflects verified reality. `READY` implies a completion has round-tripped. |
| NFR-06 | Portability | `done` | Single static Go binary. Linux and macOS. **No runtime dependencies at all** — SSH is spoken in-process rather than by driving an external client, so there is nothing to install and nothing to be missing. |
| NFR-07 | Extensibility | `part` | A new provider or runtime is added by implementing one interface, with no changes to core, ranking, wiring, or state code. |
| NFR-08 | Observability | `part` | Structured logs with a rig-scoped correlation ID; every provider request/response recorded at debug level with secrets redacted. OpenTelemetry traces and metrics cover the lifecycle, with export opt-in and the self-contained path requiring no external service. |
| NFR-09 | Testability | `live` | The full lifecycle is exercisable against fakes with zero spend. No test issues a real create. |
| NFR-10 | Licensing | `done` | GPL-3.0-or-later, copyright Sovrenix Inc. Every source file carries an SPDX short-form header. Dependencies must be GPL-3.0-compatible, audited before entering `go.mod`. |
| NFR-11 | Usability | `live` | The failure message for the most common error (no offer fits the model) states the VRAM required, the VRAM found, and the cheapest offer that would fit. |
| NFR-12 | Observability | `plan` | The telemetry plane is subordinate to the control and data planes. No telemetry failure may affect provisioning, supervision, teardown, or cost accounting. |

---

## 9. Constraints and Assumptions

1. **Provider APIs churn.** Vast.ai and RunPod change request/response shapes without
   notice. Adapters must be verified against live documentation and must fail loudly on
   unexpected shapes rather than silently mis-parsing.
2. **Interruptible instances can vanish** at any moment. Any rig on a bid/spot offer is
   presumed impermanent.
3. **Weights are large.** Multi-GB downloads dominate bootstrap time; the operator pays
   for the instance during the download.
4. **The remote host is untrusted third-party hardware.**
5. **The operator has funded provider accounts** and valid API keys. LARRI does not manage
   billing, top-ups, or account creation.
6. **No local `ssh` installation is assumed.** SSH is spoken in-process (NFR-06), and LARRI
   generates an ephemeral keypair per rig (FR-SEC-17) rather than using — or requiring the
   operator to manage — a long-lived identity.
7. **Vast.ai and RunPod have materially different models** — Vast's marketplace of offers
   with bid pricing versus RunPod's Secure/Community Cloud pod types — and the normalized
   `Offer` type must not privilege either.

---

## 10. Acceptance Criteria

### 10.1 Milestone 1 — The Manual Playbook, Automated

The seven-step Vast.ai playbook in §1 is reproduced by two commands.

- **AC-1.1** `larri up --model <model> --gpu A100 --vram 80 --max-price 1.50` provisions,
  bootstraps vLLM, and returns a working `http://127.0.0.1:8000/v1`.
- **AC-1.2** A `chat/completions` call against the local endpoint returns a real completion
  generated on the remote GPU.
- **AC-1.3** `larri status` shows state, instance, hourly price, elapsed time, accrued cost.
- **AC-1.4** `larri down` destroys the instance, confirms absence by re-query, and reports
  total cost.
- **AC-1.5** After `down`, the provider dashboard shows no running LARRI-created instance.

### 10.2 Milestone 2 — Cost Safety Under Failure

- **AC-2.1** `kill -9` on the daemon mid-`CREATING`; on restart, LARRI finds the instance
  and either adopts or destroys it. It is never lost.
- **AC-2.2** An instance created outside local state is reported by `larri orphans` and
  destroyable from it.
- **AC-2.3** A provider create call that times out but actually succeeded does not produce
  two instances.
- **AC-2.4** A breached budget ceiling triggers the configured action and is visible in
  every surface.
- **AC-2.5** An instance that the provider reports as stopped rather than absent is treated
  as billable: it is not journalled `DESTROYED`, it stays visible with its accruing storage
  cost, and `larri down` on it ends with the resource absent from the provider's inventory.
- **AC-2.6** A rig left with no operator inference for the idle timeout reaches the
  configured action, warns first with usable lead time, and does so **while health checks
  are running** — probe traffic does not hold the rig open.
- **AC-2.7** A provider API made unreachable mid-rig produces no state transition, no
  destroy, and no orphan report. When it returns, supervision resumes without having
  concluded anything.
- **AC-2.8** A stopped instance that resumes after a replacement was provisioned is detected,
  and both billing instances are reported.
- **AC-2.9** Every terminated rig — whether by operator request, idle timeout, budget breach,
  preemption, or provisioning failure — reports why, with evidence, from `larri status` alone,
  and continues to do so after the daemon has been restarted.

### 10.3 Milestone 3 — Multi-Provider, Multi-Runtime

- **AC-3.1** The same `up` invocation succeeds against both Vast.ai and RunPod, selected
  by ranking rather than by flag.
- **AC-3.2** All three runtimes serve the same `/v1` contract; the local client cannot
  tell which is behind the endpoint.
- **AC-3.3** A model too large for the offer is rejected pre-spend with the VRAM shortfall
  named (NFR-11).
- **AC-3.4** Model facts for a previously unseen model are fetched live, cached by commit, and
  reused offline on the next run. A gated model with an invalid token fails during sizing,
  before any instance is created.
- **AC-3.5** An offer whose driver cannot run any published runtime image is filtered out
  during search rather than failing after the instance is paid for.
- **AC-3.7** On a machine with no configuration file, `larri up --model <ref>` runs to
  completion without prompting, states the destructive defaults it adopted, and writes a
  configuration reflecting them. The same invocation with a non-terminal stdin behaves
  identically and never blocks.
- **AC-3.6** `larri up` configures Continue.dev for both VS Code and JetBrains from one file,
  and `larri down` restores it byte-for-byte. No client whose settings live in an
  application-owned database is written to directly.

### 10.4 Milestone 4 — Surfaces

- **AC-4.1** CLI, TUI, MCP, and web chat all report the same rig state.
- **AC-4.2** An MCP-driven agent completes the full lifecycle, with destroy confirmable.
- **AC-4.3** Preemption of an interruptible rig is detected, reported, and — with consent —
  recovered onto a new instance behind the same local port with no client reconfiguration.
- **AC-4.4** From the chat pane, asking the served model about rig health, accrued cost, and
  recent runtime logs produces answers derived from real tool calls against the daemon, and
  the same questions asked of an external agent over MCP produce the same underlying results.
- **AC-4.6** The runtime is not reachable from outside the rented host: a connection attempt
  to the instance's routable address on the runtime port is refused, while the same request
  through the tunnel succeeds.
- **AC-4.7** A request to the local endpoint without the API key is rejected, as is one
  carrying an unexpected `Host` header.
- **AC-4.8** A substituted SSH host key mid-rig stops the tunnel and marks the rig
  `DEGRADED` rather than reconnecting.
- **AC-4.9** A runtime configured with a non-loopback bind address fails to launch. No
  configuration file, flag, or environment variable produces a listening socket on a routable
  interface of the rented host.
- **AC-4.12** No provider port mapping is requested for the runtime port on either provider,
  and on RunPod no port is declared that its proxy would publish.
- **AC-4.13** A request reaching the runtime carries the rig's credential and never the
  client's. Replacing the instance rotates the rig credential with no client reconfiguration,
  and the client's token is unchanged across the replacement.
- **AC-4.14** Revoking one client's token leaves every other wired client working.
- **AC-4.15** A completion containing HTML or script markup renders as inert text in the chat
  pane, and the pane's origin holds no credential that reaches the daemon API.
- **AC-4.16** A model reference offering only pickle-format weights is rejected before any
  instance is created.
- **AC-4.17** Every file LARRI writes containing a credential is `0600`, and a symlink placed
  at a client configuration path causes the write to fail rather than be followed.
- **AC-4.10** The only port reachable from outside a running instance is SSH. Agent forwarding
  is refused, and a tunnel whose local forward fails to bind is reported as failed rather than
  reaching `READY`.
- **AC-4.11** An instance that has become unreachable over SSH is still destroyed by
  `larri down`, confirmed absent, and costed.
- **AC-4.5** A served model that emits `larri_down` does not destroy anything: the tool is
  absent from the default set, and when explicitly enabled it renders a confirmation the
  operator must accept. A model that emits tool calls in an unbounded loop is cut off by the
  depth limit with the rig unaffected.

### 10.5 Milestone 5 — Observability

- **AC-5.1** With no collector configured and no telemetry egress, the console pane renders
  live GPU, VRAM, CPU, RAM, throughput, and accrued-cost series for a running rig.
- **AC-5.2** A completed `up` produces a trace whose span durations account for the rig's
  boot time and whose per-span `larri.cost.usd` attributes sum to the boot cost recorded
  independently in the journal.
- **AC-5.3** Stalling the host metrics collector, removing the runtime `/metrics` endpoint,
  and pointing OTLP at a black hole — simultaneously, mid-rig — degrades the console and
  leaves rig state, supervision, teardown, and cost accounting unaffected.
- **AC-5.4** No telemetry signal emitted under any configuration contains prompt or
  completion content.

### Restart recovery

- **AC-6.1** With a rig serving, killing the LARRI process and running `larri resume`
  restores the endpoint **at the same local port**, and a client configured before the kill
  gets a completion afterwards with no reconfiguration.
- **AC-6.2** The resume in AC-6.1 does not restart the model: the runtime's process start
  time is unchanged across the recovery, and no weights are re-downloaded.
- **AC-6.3** No private key is written to the state directory at any point in AC-6.1.
  Inspecting every file under it after a resume finds none.
- **AC-6.4** Resume against an instance whose host key has changed fails, is classified as a
  security failure, and does **not** fall back to another host.
- **AC-6.5** Resume against an instance the provider has destroyed records the rig
  `DESTROYED` rather than retrying, and resume against a `STOPPED` one reports the ongoing
  storage cost and refuses to silently resume it.
- **AC-6.6** A failed resume names the rig's hourly cost and the command that destroys it.

---

## 11. Out of Scope

Distributed/multi-node inference; training and fine-tuning; provider account and billing
management; serving a public endpoint to third parties; model quality benchmarking or
leaderboards; Kubernetes or other orchestrators; a hosted control plane (LARRI is local,
by design and by name).

---

## 12. Risks

| ID | Risk | Impact | Mitigation |
|---|---|---|---|
| R-01 | Forgotten instance bills indefinitely. | High | FR-DEL-03…07, FR-STATE-04, budget ceilings, idle timeout, orphan sweep on every start. |
| R-02 | Provider API change breaks an adapter silently. | High | Strict decoding, contract tests against recorded fixtures, loud failure on unknown shapes. |
| R-03 | Bootstrap cost — paying for a 40-minute weight download. | Medium | Prefer pre-baked images and provider-cached weights; report download cost in progress output. |
| R-04 | Spot preemption mid-session. | Medium | FR-SUP-02, FR-WIRE-07 stable-port recovery. |
| R-05 | Client config corruption from automated edits. | Medium | Backup before write, idempotent writes, revert on `down` (FR-WIRE-04/05). |
| R-06 | Untrusted host sees prompt traffic. | Medium | Documented in FR-SEC-05; operator's informed choice. |
| R-07 | Double-provisioning from a retried create. | High | Intent-before-call (FR-PROV-01), reconcile-before-retry (FR-PROV-03). |
| R-08 | VRAM math wrong → OOM on first request after paying to boot. | Medium | Single sizing package, heavily unit-tested; conservative headroom (FR-CRIT-06). Predicted-versus-actual VRAM on the console (FR-UI-07) turns every running rig into a live check on the math. |
| R-09 | Telemetry export carries prompts or secrets off the machine. | High | FR-OBS-05 (no content, and no flag that enables it), FR-OBS-09 structural redaction, export off by default, documented attribute set. Verified by AC-5.4. |
| R-10 | A stalled metrics collector wedges provisioning or supervision. | Medium | FR-OBS-03, NFR-12: collectors are goroutine-isolated, deadline-bounded, and drop samples rather than block. Verified by AC-5.3. |
| R-11 | The served model emits a destructive or expensive tool call — through host compromise, prompt injection, or hallucination. | High | FR-UI-11: the chat pane's default tool set cannot spend, destroy, or rewire; consequential tools are opt-in and always confirmed. FR-SEC-07 treats all three causes identically rather than trying to distinguish them. Verified by AC-4.5. |
| R-12 | A tool result returns a secret to the untrusted host that LARRI deliberately withheld from it. | High | FR-SEC-06 redaction on the tool-result path, which is a distinct destination from logs, TUI, MCP results, errors, and span attributes. |
| R-13 | A stopped instance is mistaken for a destroyed one and bills storage indefinitely. | **High** | The `STOPPED` state is billable by definition (§6); FR-DEL-03 requires absence from inventory, not a status change; adapters must enumerate non-running resources, asserted by the provider conformance suite. The state name was changed from `PREEMPTED` precisely because the old one sounded terminal. |
| R-14 | A stopped interruptible resumes after a replacement was provisioned, leaving two billing instances. | High | FR-SUP-10 detection and surfacing; FR-SUP-03 keeps a replacement from appearing without operator consent; both instances carry the rig label so reconciliation finds the pair. |
| R-15 | The idle timer never fires because LARRI's own health probes look like activity. | Medium | FR-SUP-08 excludes probe traffic from the activity clock. A test asserts a rig with health checking enabled and no operator traffic still reaches the idle deadline. |
| R-16 | Maintaining pre-baked images becomes a burden as CUDA, drivers, and runtime versions move. | Medium | Accepted deliberately as the price of reproducible boots. Digest pinning keeps failures reproducible, driver-aware selection keeps incompatible hosts out of the search, and the stock-image fallback is exercised in tests so it does not rot when unused. |
| R-17 | Live model-fact fetching makes sizing depend on Hugging Face being reachable. | Medium | Cache keyed by resolved commit, so hits are facts about immutable revisions; cache-only operation is a supported mode; operator override always wins; unresolvable facts are a hard error rather than a guess. |
| R-18 | The runtime is reachable from the internet on a rented host, and strangers spend the operator's GPU. | **High** | Two layers that always hold: FR-SEC-15 requests a port mapping for SSH only (provider-enforced, primary), and FR-SEC-08 binds loopback and forbids configuring otherwise. Everything else rides the tunnel. Direct port-mapping is off by default (FR-SEC-12). |
| R-19 | A network MITM intercepts the tunnel because host key checking was skipped. | High | FR-SEC-04 mandatory pinning in a LARRI-owned `known_hosts`; the insecure SSH flags are never emitted; a mid-rig key change is treated as compromise. Residual TOFU window on first connect is narrow but real and is stated rather than papered over. |
| R-20 | A local process or a web page in the operator's browser drives the endpoint and spends money. | Medium | FR-SEC-09 mandatory local API key and `Host` validation; loopback is explicitly not treated as a trust boundary. |
| R-21 | SSH is a single point of access; if sshd fails, the data plane is gone. | Medium | Accepted deliberately — every fallback path is also an attack surface. Supervision detects it and the rig goes `DEGRADED`; recovery is replacement, not a second door. FR-SEC-18 keeps teardown independent of SSH, so lost access can never produce an instance that bills forever. |
| R-22 | Agent forwarding to a rented host lets its operator authenticate as the user elsewhere. | **High** | FR-SEC-16: agent forwarding is *not implemented*, and the operator's SSH config — where a global `ForwardAgent yes` would otherwise apply — is never read. FR-SEC-17's ephemeral per-rig key means the identity presented to a rented host is not reusable anywhere else. |
| R-23 | A declared port is auto-published by a provider proxy, exposing the runtime to the internet. | High | FR-SEC-19 declares SSH only; FR-SEC-08's loopback bind means even a mistaken declaration reaches a port with nothing listening. Verified by AC-4.12. |
| R-24 | The proxy forwards the client's credential to the rented host, disclosing a token shared by every wired client. | High | FR-SEC-22 makes stripping and substitution explicit, because header pass-through is the default behaviour of most reverse proxy implementations and would do exactly this. Verified by AC-4.13. |
| R-25 | Model output is rendered as HTML in a page that can reach the daemon API, giving an untrusted host XSS with control-plane reach. | **High** | FR-SEC-31: safe-subset markdown, strict CSP, and separate origins for the chat and console panes so XSS in the pane rendering untrusted content cannot reach control-plane credentials. |
| R-26 | Weights in a pickle-based checkpoint execute arbitrary code on the rented host during load. | **High** | FR-SEC-29 requires `safetensors` and refuses pickle formats; the resolved commit is pinned. Costs nothing, since every supported runtime prefers `safetensors`. |
| R-27 | The ranking function steers operators toward a malicious host, because price is its heaviest term and a host seeking victims lists cheaply. | Medium | FR-SRCH-08 flags anomalous underpricing; reliability weighting counters fresh listings weakly; `Criteria.CertifiedOnly` is the strong control for workloads the operator would not publish. Inherent to anonymous marketplaces, so stated rather than solved. |
| R-28 | Weights and KV cache remain in GPU memory for the next tenant of the machine. | Medium | **Accepted.** Teardown cannot verify driver-level scrubbing from outside the host. Documented in §15.8 rather than mitigated. |

---

## 13. Open Questions

**None.** Every question raised in this document has been answered by the operator and
recorded in §13.1 with its reasoning. New questions are added here rather than assumed away;
a decision made silently is the failure this section exists to prevent.

### 13.1 Resolved

Recorded with the reasoning, not just the verdict — a resolution whose argument is lost gets
relitigated the first time it is inconvenient.

| ID | Question | Resolution |
|---|---|---|
| Q-01 | Which IDE/chat clients are wired automatically in v1, and where do their configs live? | **2026-08-21.** IDEs: **Continue.dev** (`~/.continue/config.yaml`) covers VS Code *and* JetBrains from one file, plus **VS Code native Copilot Chat BYOK**. Chat: **LibreChat** is the primary target — `librechat.yaml` exists precisely to register OpenAI-compatible endpoints — with **Open WebUI** supported conditionally and **AnythingLLM** retained (self-hosted by file, desktop guided). Clients are classified by writability tier (FR-WIRE-10) so that "cannot be safely automated" is a supported outcome rather than a reason to write recklessly. **Cursor is excluded structurally**: it proxies model traffic through its own backend, so a loopback endpoint is unreachable and support would mean publicly exposing the rig, contradicting FR-SEC-03 and the out-of-scope exclusion. See LARRI-DES-001 §10.2.2. |
| Q-02 | Is the web chat UI a first-class surface in v1, or does an existing chat client fill that role? | **2026-08-21 — both, split into two panes.** The web surface is served from Go with no Node toolchain: stdlib templates, vanilla JS, uPlot, `go:embed`. The **console pane** (FR-UI-07) is first-class, because nothing else provides live cost-against-utilisation for a rented rig. The **chat pane** (FR-UI-05) is deliberately minimal — enough to confirm a rig serves and hold a conversation. Rich chat is delegated to existing clients wired by FR-WIRE. A React/AI-SDK build was rejected: `useChat` speaks the AI SDK UI message stream protocol rather than raw OpenAI SSE, so its intended architecture needs a Node server route at runtime, which NFR-06 forbids. See LARRI-DES-001 §14.4. |
| Q-03 | Default posture on budget breach — warn, or auto-destroy? | **2026-08-21 — destroy**, consistent with Q-11, after a warning with usable lead time. The ceiling counts storage accrued by `STOPPED` rigs, not GPU time alone, or the leak in R-13 never trips it. Breach records a typed termination reason with the ceiling, the accrued total, and the crossing sample. Ceilings exist per rig and globally, which look identical while N=1 and would be conflated by a design that assumed one rig forever. See §12.5. |
| Q-04 | Are interruptible/bid offers enabled by default, or opt-in? | **2026-08-21 — opt-in** (`Interruptible` defaults to `forbid`). This session's finding changed the answer: a preempted Vast.ai instance is *stopped, not destroyed* — still billing storage, sometimes at a higher rate, and able to resume by itself into a second billing instance (R-13, R-14). A 0.15 ranking penalty prices the inconvenience of preemption, not a storage-billing husk someone has to notice and reap. Still fully supported, and often correct for a deliberate watched session — which is what `--interruptible` expresses. |
| Q-05 | Multiple concurrent rigs in v1, or one at a time? | **2026-08-21 — one at a time, by configuration, not by architecture.** `max_concurrent_rigs: 1`; state, API, supervision, and reconciliation stay keyed by rig. The one thing that cannot be deferred is how clients address several rigs behind one fixed port, since retrofitting it would reconfigure every client — exactly what P3 exists to prevent. Resolved by routing the canonical port on **served-model name** (FR-WIRE-09): a pass-through at N=1, and at N>1 rig selection becomes a model dropdown the client already has. See §10.3. |
| Q-06 | Where do model facts come from — bundled catalogue, live fetch, or both? | **2026-08-21 — live fetch from Hugging Face**, cached by resolved commit, with operator override winning and unresolvable facts a hard error. `config.json` is the same file the runtime will read; a bundled catalogue drifts from the day it ships. Two consequences are features: a gated repo exercises `HF_TOKEN` during sizing, so a bad token fails before anything is billed; and GGUF repos without a usable config fall back to the file's own metadata, then to an explicitly recorded base model — never to inference from a repo name. See §7.1. |
| Q-07 | Pre-baked project image, or stock images? | **2026-08-21 — pre-baked, pinned by digest.** A stock image plus a boot-time install is a fresh dependency resolution on someone else's hardware on a billing clock, and R-03 says that clock is the expensive part. The image carries the runtime, matching CUDA userspace, and the host-metrics tooling §17.4 needs; weights stay the one large download. Variant selection is driven by the host's reported driver, making it a **search filter** rather than a post-create discovery — which is why `Offer` now carries `CUDAVersion`. Stock fallback stays supported and tested. Pre-baking buys reproducibility, not trust: the host still controls what runs (FR-SEC-05). See §6.5. |
| Q-08 | Does the metrics ring buffer survive a daemon restart? | **2026-08-21 — persisted**, downsampled as it ages (1 s live → 10 s → 60 s), retained in step with terminated-rig retention. The reason is not restart resilience but post-mortem completeness: "destroyed on idle timeout" beside a utilisation trace that flatlined forty minutes earlier is an explanation; without the trace it is an assertion. Append-only JSONL, because `CGO_ENABLED=0` rules out SQLite and this needs no migration story. Writes are best-effort and subordinate to FR-OBS-03. See §17.5. |
| Q-09 | When no rig is serving, may the chat pane fall back to another endpoint? | **2026-08-21 — no. The chat pane is disabled.** It states that no rig is serving and offers no fallback to a local Ollama or a hosted API. A fallback would change who the operator is talking to at the moment they are least likely to notice, and would add a second inference dependency to a tool whose premise is that you bring your own. Control during an outage belongs to the MCP surface and the console pane, neither of which depends on the rig. See LARRI-DES-001 §14.4.4. |
| Q-10 | Are consequential tools offered to the chat pane at all, even behind confirmation? | **2026-08-21 — yes, opt-in and always confirmed.** The default tool set cannot spend, destroy, or rewire; `larri_up`, `larri_down`, and `larri_orphan_destroy` are excluded unless the operator enables them, and each call then renders a confirmation stating the operation and its cost implication. Auto-execution is never permitted for a consequential tool, regardless of driver. See FR-UI-11. |
| Q-11 | Does idle reclamation default to `destroy`, or stay `warn` until the operator has seen it behave once? | **2026-08-21 — `destroy`, and explain itself.** Forgetting is the failure the product exists to prevent, so the default reclaims. The safeguard is not timidity but accountability: the reclamation records a typed termination reason with evidence (FR-DEL-08) — last operator request, configured window, actual idle duration, and the note that health probes were excluded from the activity clock — and the terminated rig is retained so the operator can inspect it later (FR-DEL-09). A destruction that can explain itself is safe to automate; one that cannot is not. See LARRI-DES-001 §13.1. |
