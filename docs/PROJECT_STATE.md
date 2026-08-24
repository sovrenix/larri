# LARRI — project state

**Version 0.9.0.** Generated from the status column in
[LARRI_Requirements_Specification.md](LARRI_Requirements_Specification.md), which is the
source of truth. If this file and that one disagree, that one is right and this one is stale.

| Status | Means |
|---|---|
| ✅ `live` | Implemented **and** exercised against real hardware or a real provider API |
| ☑️ `done` | Implemented and unit-tested |
| 🟡 `part` | Partially implemented — the gap is named below |
| ⬜ `plan` | Specified, not started |

The `live` / `done` split is the one that matters. This project has repeatedly shipped code
that passed its unit tests and then failed on real hardware — three deadlines measuring
elapsed time instead of silence, a daemon reading one engine's log path whatever was running,
a `pkill` that matched its own shell three separate times, a start command handed to a
runtime as flags because `dockerStartCmd` overrides CMD and not ENTRYPOINT. "Implemented"
and "shown to work" are different claims and are recorded as such.

---

## Summary

| Area | Reqs | ✅ live | ☑️ done | 🟡 part | ⬜ plan | Complete |
|---|---:|---:|---:|---:|---:|---:|
| Criteria | 6 | 2 | 4 | 0 | 0 | **100%** |
| Provisioning | 6 | 5 | 1 | 0 | 0 | **100%** |
| State | 5 | 4 | 1 | 0 | 0 | **100%** |
| Search & selection | 10 | 6 | 3 | 0 | 1 | **90%** |
| Runtimes | 15 | 11 | 2 | 1 | 1 | 87% |
| Non-functional | 12 | 6 | 4 | 1 | 1 | 83% |
| Configuration | 10 | 0 | 8 | 1 | 1 | 80% |
| Teardown & cost safety | 11 | 3 | 5 | 3 | 0 | 73% |
| Supervision | 18 | 7 | 6 | 4 | 1 | 72% |
| Security | 31 | 13 | 7 | 2 | 9 | 65% |
| Surfaces | 13 | 1 | 2 | 3 | 7 | 23% |
| Endpoint & client wiring | 14 | 3 | 0 | 1 | 10 | 21% |
| Observability | 10 | 0 | 1 | 0 | 9 | 10% |
| **Total** | **161** | **61** | **44** | **16** | **40** | **65%** |

Sixty-five per cent of requirements are implemented, and **the lifecycle is the part that is
done.** Renting, sizing, serving, supervising, reconnecting and tearing down are at or near
100%; what is missing clusters into three areas missing almost entirely — client wiring, the
browser surfaces, and observability.

Note where the movement has been. Total completion has barely shifted, but 61 requirements
are now `live` against 57 a fortnight ago — the work went into *proving* things that were
already written, on a second provider that had never run them.

---

## What works, end to end

**Two providers, both verified at real cost.**

| | Vast.ai | RunPod |
|---|---|---|
| Rent → serve → prompt → destroy | ✅ ~20 paid runs | ✅ RTX 4090, $0.0145, 2m34s |
| Absence confirmed after destroy | ✅ | ✅ |
| Boot log during bring-up | via SSH once reachable | ✅ also over the provider's log API, before SSH exists |
| `larri resume` after a restart | ✅ | ❌ no attach-key API — refuses, see below |

- **Three runtimes**: vLLM, llama.cpp and Ollama, all live. llama.cpp and Ollama on GTX 1060s
  at under two cents an hour — hardware the vLLM path cannot use at any price.
- **Cheapest-that-passes-the-floors selection** across a ~2,400-offer market, and across
  RunPod's catalogue, which is a price list rather than a market and needed the ranking
  inputs to tolerate facts a catalogue does not publish.
- **Automatic VRAM sizing** from live Hugging Face facts, GGUF headers, or an Ollama registry
  manifest — no `A100:1` to type.
- **Reconnection after the process dies**: a fresh SSH key installed through the provider
  API, host key verified against the recorded fingerprint, same local port, same pid — no
  private key ever written to disk.
- **MCP**: an agent rents, polls to ready, prompts the model, reads logs and destroys, with
  the rig held across calls by a session rather than re-derived per tool.
- **Cost safety**: intent journalled before spend, orphan sweep by provider-side label, idle
  and budget enforcement, typed termination records, host-side dead-man switch.

### The second provider is the real news

RunPod went from nothing to live in one cycle, and the interesting part is what *didn't*
change: the provider interface took no new methods. Three optional capability interfaces
absorbed the differences — `KeyAttacher` (Vast has it, RunPod doesn't), `Reporter`
(explaining why an offer was dropped), `BootLogger` (RunPod streams container logs over its
API, so bring-up is observable before sshd exists, which is precisely when it usually fails).
A provider declines a capability by not implementing it, and the daemon adapts. No core code
branches on a provider name.

Two negative results were recorded rather than worked around: RunPod's proxy SSH carries no
port forwarding, so the tunnel needs the direct TCP path (FR-SEC-20 asked for this to be
determined empirically — it now has been); and RunPod's template `startSsh` flag injects the
*deployer's account-registered* keys, which contradicts the ephemeral-per-rig key in
FR-SEC-17, so templates were evaluated and rejected in favour of installing sshd on boot
(§5.5).

---

## 🟡 Partial — what is missing, specifically

Sixteen requirements are partly met. The gap for each:

| ID | Gap |
|---|---|
| FR-RT-10 | Tool-calling parsers are passed through; **refusing** a rig when tool calling is required and unavailable is not enforced. |
| FR-SUP-03 | Fallback picks the next-ranked offer without comparing its price to the original, so a silent upgrade is possible. |
| FR-SUP-05 | Budget ceilings are **per rig**; there is no global ceiling across rigs. |
| FR-SUP-09 | Deadline warnings reach the CLI and TUI; "every surface" is not met while surfaces are missing. |
| FR-SUP-10 | `STOPPED` is detected and surfaced; **resume detection** — spotting that a stopped rig came back alongside its replacement — is not. |
| FR-DEL-01 | `down` destroys and reports cost but reverts no client wiring, because none is written. |
| FR-DEL-07 | `larri orphans --destroy` sweeps the selected provider; a **single cross-provider panic sweep** is now meaningful with two providers configured, and is not built. |
| FR-DEL-10 | The termination reason reaches CLI, TUI and MCP; the missing surfaces cannot show it. |
| FR-CFG-05 | Assumed defaults are disclosed at bring-up but not recorded for after-the-fact audit. |
| FR-SEC-01 | Keys resolve from the **environment**; OS keyring support is not implemented. |
| FR-SEC-13 | The Hugging Face token sent to a host is the operator's own, not a scoped read-only credential. |
| FR-WIRE-09 | The proxy carries a served-model name but routes a single upstream; multi-rig routing is not built. |
| FR-UI-01 | `up`, `down`, `status`, `offers`, `orphans`, `config`, `resume`, `mcp`, `tui` ship; **no `logs` command** (it exists only as an MCP tool) and no `daemon`. |
| FR-UI-06 | Surfaces share state through one store, but with no daemon there is no cross-process consistency guarantee. |
| FR-UI-11 | The tool registry enforces the safe/consequential split; there is no chat pane to apply it to. |
| NFR-08 | Structured events and an append-only journal exist; no rig-scoped correlation ID is plumbed through. |

---

## ⬜ Not started

**Endpoint & client wiring (10).** Continue.dev, VS Code, LibreChat and the rest are not
configured automatically; the writability tiers, post-write verification and revert-on-`down`
are all specified and unbuilt. Configure clients by hand against the printed endpoint.

**Surfaces (7).** No daemon, no web console, no chat pane, and none of the chat-driven
control that depends on them.

**Observability (9).** No OpenTelemetry, no collectors, no metric store. The one exception is
FR-OBS-04 — cost and the audit trail already derive from the journal rather than from
telemetry, which is the property the rest is designed around.

**Security (9).** Everything outstanding here belongs to surfaces that do not exist: browser
credential handling, chat-pane tool redaction, the daemon socket. Two are independent and
real: **image signing** (FR-SEC-30) and **rate limiting on the local endpoint** (FR-SEC-14).

**Runtimes (1).** Pre-baked, digest-pinned images (FR-RT-11). Stock upstream images are used
instead, which is why bring-up depends on discovering the launcher at runtime.

---

## The risks worth naming

**Teardown cannot be guaranteed against local process death.** Measured, not assumed: a
marketplace container cannot end its own billing — `CAP_SYS_BOOT` is absent from its
capability bound and signalling PID 1 achieves nothing (§12.4.1). The host watchdog is
therefore *containment* — an abandoned rig stops serving — and `larri orphans` is the only
thing that stops it costing.

**A RunPod rig is destroyable but not reconnectable.** `larri resume` needs a fresh key
installed on an already-running instance, and RunPod exposes no such call, so it refuses with
that phrase rather than pretending (FR-SUP-16, live-verified on both branches: Vast
reconnects, RunPod declines). Teardown never depended on SSH, so the rig stays killable —
which is the property that makes the gap tolerable rather than dangerous.

**On RunPod, sshd faces the public internet.** Vast fronts SSH with its own proxy; RunPod
maps port 22 onto a routable address, so the distribution default is not a detail — it is the
configuration facing the internet. The start command now writes an explicit `sshd_config.d`
drop-in before starting the daemon: no password auth, no empty passwords, no
challenge-response, root by key only. Combined with the ephemeral per-rig key and host-key
pinning, the exposed surface is one hardened daemon holding one key that dies with the rig.
Port 22 is still the *only* port declared — the runtime binds remote loopback and rides the
tunnel (FR-SEC-19, now live on both providers).

---

## Milestones

| M | Scope | State |
|---|---|---|
| **M0** | Foundations, fakes, CI gates | ✅ complete |
| **M1** | One rig, safely | ✅ complete, live-verified |
| **M2** | Cost safety under failure | 🟡 crash injection and preemption recovery outstanding |
| **M3** | Breadth | ✅ three runtimes and **two providers**, all live-verified |
| **M4** | Surfaces | 🟡 MCP and TUI done; daemon, web UI, client writers outstanding |
| **M5** | Observability | ⬜ not started |

**M3 is complete** — that was 1.0's blocker. What stands between here and 1.0 is now M2's
failure-injection work and M4's client wiring, which is the half of the product the operator
actually touches: today `larri up` prints an endpoint and you configure your editor by hand.
See §20.0.1.
