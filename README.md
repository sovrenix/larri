<div align="center">

```
                      \        /
                       \      /
       __/\__           \    /             __/\__
      /      \           \  /             /      \
      |  ()  |            \/              |  ()  |
      \      /         ,------,           \      /
       \    /         /  o  o  \           \    /
        \__/          |   ..   |            \__/
         \____________|________|_____________/
                      |########|
                      |########|
                      |########|
                      \########/
                       \______/
                        \ || /
                        / || \
                      (__/  \__)
```

# L.A.R.R.I.

**Local Agent for Remote Rigging of Inference**

*Rent a GPU. Serve a model. Stop paying. Two commands.*

[![Licence: GPL v3](https://img.shields.io/badge/Licence-GPLv3-blue.svg)](LICENSE)
[![Status: design](https://img.shields.io/badge/Status-design%20stage-orange.svg)](#status)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8.svg)](https://go.dev)

</div>

---

> ### 🦞 Why a lobster?
>
> A lobster outgrows its shell and walks away from it — same lobster, new shell, no fuss.
> That is precisely what LARRI does with rented hardware. The GPU you are renting is a shell:
> temporary, replaceable, and not where your identity lives. Your tools stay pointed at one
> unchanging local address while LARRI moults from one rented machine to the next underneath
> them.
>
> The claws are for holding on to your wallet.

---

## Status

**This project is at the design stage. There is no working code yet.**

What exists today is a complete, reviewed specification: a [Requirements
Specification](docs/LARRI_Requirements_Specification.md) and a [Design
Document](docs/LARRI_Design_Document.md) covering architecture, the provider and runtime
abstractions, VRAM sizing mathematics, cost-safety invariants, observability, and a full
threat model with attack-surface analysis. Every open design question has been resolved and
recorded with its reasoning.

The commands shown below describe the intended interface. They do not run yet. Milestone M1
is the first implementation step.

---

## What LARRI is

LARRI rents someone else's GPU, stands up an inference server on it, points your local tools
at it, and keeps it alive only as long as you want it.

```
criteria → search offers across providers → rank → provision → bootstrap runtime
        → wait for readiness → publish a stable local endpoint → rewire IDE + chat
        → supervise → destroy on command
```

Providers are **Vast.ai** and **RunPod**. Runtimes are **vLLM**, **llama.cpp**, and
**Ollama**. The whole thing is a single static Go binary with no runtime dependencies.

### The problem

Standing a model up on rented hardware by hand is a seven-step playbook: pick an instance,
SSH in, install the runtime, authenticate to Hugging Face, launch the server with the right
flags, forward the port, reconfigure every local client — and then remember to destroy it.

Every step is manual and error-prone. The last one costs money when it is forgotten, which
it will be. LARRI collapses the playbook into `larri up` and `larri down`, and the guarantee
that matters is the second one.

### What makes it different from a wrapper script

- **A rented GPU bills by the second whether or not LARRI is running.** Intent is written to
  disk *before* the call that spends. Destruction is confirmed by re-querying the provider,
  because a `200` from a delete endpoint is a claim and absence from the inventory is
  evidence.
- **"Stopped" is not "gone."** A preempted instance still exists and still bills for storage.
  LARRI treats that as a billable state requiring a decision, not a terminal one.
- **VRAM is computed, not guessed.** `(model, quantisation, context length)` → required VRAM
  is one function used by the search filter, the ranking function, and the launch flags, so
  you cannot over-commit in one place and not the others.
- **Rigs that die explain themselves.** Every teardown records who decided, why, and the
  evidence — inspectable long afterwards.
- **The rented host is treated as untrusted, because it is.** See [Security](#security).

---

## How to use

> Not yet implemented — this is the intended interface.

### Bring up a rig

```bash
larri up --model Qwen/Qwen3-Coder-30B \
         --quantization q4_K_M \
         --context 32768 \
         --gpu A100 --max-price 1.50
```

```
  sizing    Qwen3-Coder-30B @ q4_K_M, 32768 ctx
            weights 19.1 GB · kv-cache 4.2 GB · overhead 2.1 GB → 27.9 GB required
  search    vastai 214 offers · runpod 38 offers → 19 satisfy criteria
  ranked    1. vastai  A100 80GB  $1.29/hr  fit 0.35  rel 0.98  score 0.81
  → select  vastai #9182736 @ $1.29/hr   [confirm? y]
  boot      image ✓ · weights 19.1 GB ████████████ 100% (4m12s, $0.09)
  tunnel    127.0.0.1:8000 → 14872213:8000
  ready     completion round-trip 412ms ✓
  wire      2 clients configured (backed up)

  ✓ rig 01J9Z… READY   http://127.0.0.1:8000/v1   model: qwen3-coder
    $1.29/hr · elapsed 6m04s · accrued $0.13
```

### Everyday commands

| Command | Does |
|---|---|
| `larri up` | Search, rank, provision, bootstrap, wire clients |
| `larri status` | State, price, elapsed time, accrued cost — and why a past rig ended |
| `larri down` | Revert wiring, destroy, **confirm absence**, report total cost |
| `larri offers` | Search and rank without spending anything |
| `larri orphans` | Find and destroy resources that local state does not account for |
| `larri ui` | Web console: live KPIs, GPU/VRAM graphs, accrued cost, chat pane |
| `larri mcp` | Expose the lifecycle as MCP tools for Claude Code and other agents |

### Not spending money by accident

```bash
larri up --idle-timeout 30m --idle-action destroy   # default
larri up --budget 5.00                              # destroys on breach, after warning
```

Both explain themselves afterwards:

```
  ⚠ rig 01J9Z… DESTROYED   ran 2h14m · total $2.87
    policy: idle-timeout · no operator inference for 31m (window 30m)
    last request 13:51:04 · inspect with: larri status 01J9Z…
```

### Your tools, configured once

LARRI writes the stable loopback endpoint into clients it detects, backs them up first, and
reverts them exactly on `down`:

| Client | Covers |
|---|---|
| **Continue.dev** | VS Code **and** JetBrains, from one config file |
| **VS Code** | Native Copilot Chat BYOK |
| **LibreChat** | Primary chat target |
| **Open WebUI**, **AnythingLLM** | Supported with caveats |

---

## Architecture

```
 LOCAL MACHINE                                      RENTED HOST
┌────────────────────────────────────┐        ┌──────────────────────┐
│  CLI    TUI    MCP    Web UI       │        │  Runtime process     │
│    └──────┴──────┴──────┘          │        │  vLLM / llama.cpp /  │
│           │ HTTP over unix socket  │        │  Ollama              │
│    ┌──────▼───────────────┐        │        │  127.0.0.1:8000/v1   │
│    │      DAEMON          │        │        └──────────┬───────────┘
│    │  orchestrator        │        │                   │
│    │  supervisor          │        │        ┌──────────┴───────────┐
│    │  cost accountant     │        │        │  sshd                │
│    ├──────────────────────┤        │        └──────────┬───────────┘
│    │ state │ rank │ sizing│        │                   │
│    │ provider│runtime│wire│        │                   │
│    └───┬──────────────┬───┘        │                   │
│        │              │            │                   │
│  ┌─────▼─────┐  ┌─────▼─────────┐  │                   │
│  │state store│  │tunnel + proxy │──┼─── SSH ───────────┘
│  │~/.local/  │  │127.0.0.1:8000 │  │
│  └───────────┘  └───────▲───────┘  │   ┌────────────────────┐
│                         │          │◄─►│ Provider API       │
│  IDE + chat clients ────┘          │   │ Vast.ai / RunPod   │
│  (configured once, never rewired)  │   └────────────────────┘
└────────────────────────────────────┘
```

Two abstractions, and only two. **Provider** normalises Vast's offers and RunPod's pods into
one vocabulary. **Runtime** hides how weights are fetched, how VRAM fit is computed, and what
"ready" means. Nothing above the runtime layer knows which engine is serving.

Three planes, deliberately separated:

| Plane | Carries | Property |
|---|---|---|
| **Control** | daemon ↔ provider API | Low volume, high consequence, every call reconcilable |
| **Data** | client → loopback → tunnel → runtime | High volume, unaware of the control plane |
| **Telemetry** | rig → daemon → console | Subordinate: may never affect the other two |

```
cmd/larri/            CLI entrypoint and subcommands
internal/provider/    Provider interface + vastai/, runpod/
internal/runtime/     Runtime interface + vllm/, llamacpp/, ollama/
internal/sizing/      VRAM / KV-cache / context mathematics
internal/rank/        Offer scoring
internal/state/       Durable store, journal, reconciliation
internal/wire/        Tunnel, proxy, client config writers
internal/daemon/      Orchestrator, supervisor, cost accountant, API
internal/telemetry/   OpenTelemetry, collectors, metrics store
internal/webui/       Embedded chat pane + KPI console
```

Full detail in the [Design Document](docs/LARRI_Design_Document.md).

---

## Security

LARRI assumes **the machine you rented belongs to someone else, and they have root on it.**
That assumption shapes the design rather than decorating it:

- The runtime binds **loopback on the rented host** and no port is mapped for it. The only
  service reachable from outside the instance is SSH. This is not configurable.
- SSH is spoken **in-process**, so agent forwarding, X11, and remote forwarding are *not
  implemented* rather than disabled by a flag someone could override.
- **Ephemeral keypair per rig.** Your long-lived identity never reaches rented hardware, and
  revocation is automatic.
- **Two credentials with opposite lifetimes.** The local proxy strips the client's token and
  substitutes the rig's, so a client credential never reaches a host and a rig credential is
  never visible to a client.
- **Model output is untrusted content**, not merely untrusted tool calls.

And the honest limit: none of this protects you from the host operator, who can read GPU
memory. Confidential computing is the only real answer and is not available on commodity
marketplace hardware. **Do not send a rented GPU anything you could not afford to disclose.**

The complete threat model and attack-surface analysis — including gaps that are accepted
rather than solved — is §15 of the [Design Document](docs/LARRI_Design_Document.md).

---

## Contributing

Contributions are welcome. A few things worth knowing first:

1. **Read the two documents.** They are the ground truth, and they explain *why* things are
   the way they are. When code and the documents disagree, one of them gets fixed
   deliberately — divergence is not left to accumulate.
2. **Cost safety wins every argument.** No code path may leave a billable resource
   unrecorded. Where this conflicts with any other property, it wins.
3. **No test may issue a real `create`.** The Provider and Runtime interfaces are narrow
   precisely so they can be faked. Live end-to-end runs are build-tagged, env-gated, and tear
   down in a `defer` that also runs on panic.
4. **Every source file carries an SPDX header** (see below).

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./... && gofmt -l .    # gofmt must print nothing
```

Sign your commits off under the [Developer Certificate of Origin](https://developercertificate.org/):

```bash
git commit -s
```

Contributors retain copyright in their contributions and licence them under GPL-3.0-or-later.
There is no CLA.

---

## Licence

Copyright © 2026 **Sovrenix Inc.**

LARRI is free software, licensed under the **GNU General Public License v3.0 or later**. See
[LICENSE](LICENSE) for the full text and [NOTICE](NOTICE) for the canonical copyright notice.

This program is distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY;
without even the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.

Every source file begins with:

```go
// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later
```

<div align="center">
<sub>🦞 <b>Larri the Lobster</b> — same lobster, new shell.</sub>
</div>
