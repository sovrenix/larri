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
a `pkill` that matched its own shell three separate times. "Implemented" and "shown to work"
are different claims and are recorded as such.

---

## Summary

| Area | Reqs | ✅ live | ☑️ done | 🟡 part | ⬜ plan | Complete |
|---|---:|---:|---:|---:|---:|---:|
| Criteria | 6 | 2 | 4 | 0 | 0 | **100%** |
| Provisioning | 6 | 4 | 2 | 0 | 0 | **100%** |
| State | 5 | 4 | 1 | 0 | 0 | **100%** |
| Runtimes | 15 | 11 | 2 | 1 | 1 | 87% |
| Configuration | 10 | 0 | 8 | 1 | 1 | 80% |
| Non-functional | 12 | 6 | 3 | 2 | 1 | 75% |
| Teardown & cost safety | 11 | 3 | 5 | 3 | 0 | 73% |
| Supervision | 18 | 6 | 7 | 4 | 1 | 72% |
| Search & selection | 10 | 6 | 1 | 2 | 1 | 70% |
| Security | 31 | 11 | 9 | 2 | 9 | 65% |
| Surfaces | 13 | 1 | 2 | 3 | 7 | 23% |
| Endpoint & client wiring | 14 | 3 | 0 | 1 | 10 | 21% |
| Observability | 10 | 0 | 1 | 0 | 9 | 10% |
| **Total** | **161** | **57** | **48** | **16** | **40** | **63%** |

Sixty-five per cent of requirements are implemented, and **the lifecycle is the part that
is done.** Renting, sizing, serving, supervising, reconnecting and tearing down are at or
near 100%; what is missing clusters into three areas that are missing almost entirely —
client wiring, the browser surfaces, and observability.

---

## What works, end to end

Verified repeatedly against Vast.ai at real cost:

- **Rent → serve → prompt → destroy**, with absence confirmed at the provider and the cost
  reported. Roughly a dozen paid runs.
- **Three runtimes**: vLLM, llama.cpp and Ollama, all live. llama.cpp and Ollama on GTX 1060s
  at under two cents an hour — hardware the vLLM path cannot use at any price.
- **Cheapest-that-passes-the-floors selection** across a ~2,400-offer market.
- **Automatic VRAM sizing** from live Hugging Face facts, GGUF headers, or an Ollama
  registry manifest — no `A100:1` to type.
- **Reconnection after the process dies**: a fresh SSH key installed through the provider
  API, host key verified against the recorded fingerprint, same local port, same pid — no
  private key ever written to disk.
- **MCP**: an agent rents, polls to ready, prompts the model, reads logs and destroys.
- **Cost safety**: intent journalled before spend, orphan sweep by provider-side label,
  idle and budget enforcement, typed termination records.

---

## 🟡 Partial — what is missing, specifically

Nineteen requirements are partly met. The gap for each:

| ID | Gap |
|---|---|
| FR-RT-10 | Tool-calling parsers are passed through; **refusing** a rig when tool calling is required and unavailable is not enforced. |
| FR-SUP-03 | Fallback picks the next-ranked offer without comparing its price to the original, so a silent upgrade is possible. |
| FR-SUP-05 | Budget ceilings are **per rig**; there is no global ceiling across rigs. |
| FR-SUP-09 | Deadline warnings reach the CLI and TUI; "every surface" is not met while surfaces are missing. |
| FR-SUP-10 | `STOPPED` is detected and surfaced; **resume detection** — spotting that a stopped rig came back alongside its replacement — is not. |
| FR-DEL-01 | `down` destroys and reports cost but reverts no client wiring, because none is written. |
| FR-DEL-07 | `larri orphans --destroy` sweeps one provider; a cross-provider panic operation is not meaningful yet. |
| FR-DEL-10 | The termination reason reaches CLI, TUI and MCP; the missing surfaces cannot show it. |
| FR-CFG-05 | Assumed defaults are disclosed at bring-up but not recorded for after-the-fact audit. |
| FR-SEC-01 | Keys resolve from the **environment**; OS keyring support is not implemented. |
| FR-SEC-13 | The Hugging Face token sent to a host is the operator's own, not a scoped read-only credential. |
| FR-WIRE-09 | The proxy carries a served-model name but routes a single upstream; multi-rig routing is not built. |
| FR-UI-01 | `up`, `down`, `status`, `offers`, `orphans` ship; **no `logs` command** (it exists only as an MCP tool) and no `daemon`. |
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

## The two risks worth naming

**RunPod can search but cannot yet serve.** Its API contract is now fully verified against
the live service — search, create, get, list and destroy all pass the conformance suite with
a real key, and `larri offers --provider runpod` works without an account at all. What blocks
`larri up` is not the adapter: **RunPod supplies no SSH**, so a pod is reachable only if its
image runs `sshd`, and upstream engine images do not (`vllm/vllm-openai:latest` has no `sshd`
binary). Vast supplies SSH itself, which is why this never came up. The fix is FR-RT-11 —
project-maintained images carrying the engine *and* sshd — which was `plan` and is now a
prerequisite.

*(The abstraction question this entry used to raise is answered: adding RunPod changed the
`Offer` contract — reliability became optional, host exclusion became conditional — but not
the `Provider` interface itself. §5.4 records what had to move.)*

**Teardown cannot be guaranteed against local process death.** Measured, not assumed: a
marketplace container cannot end its own billing — `CAP_SYS_BOOT` is absent from its
capability bound and signalling PID 1 achieves nothing (§12.4.1). The host watchdog is
therefore *containment* — an abandoned rig stops serving — and `larri orphans` is the only
thing that stops it costing.

---

## Milestones

| M | Scope | State |
|---|---|---|
| **M0** | Foundations, fakes, CI gates | ✅ complete |
| **M1** | One rig, safely | ✅ complete, live-verified |
| **M2** | Cost safety under failure | 🟡 crash injection and preemption recovery outstanding |
| **M3** | Breadth | 🟡 three runtimes live; **RunPod not started** |
| **M4** | Surfaces | 🟡 MCP and TUI done; daemon, web UI, client writers outstanding |
| **M5** | Observability | ⬜ not started |

**1.0 waits on M3's provider half**, not on polish. See §20.0.1.
