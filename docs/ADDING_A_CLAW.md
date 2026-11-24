# Adding a claw

A **claw** is an application LARRI rents hardware for. `internal/claw` holds the
contract; `internal/claw/<name>` holds one implementation each. This is the how-to.
The *why* lives in invariant 1 of [CLAUDE.md](../CLAUDE.md) and in §6.8 of the
[design document](LARRI_Design_Document.md) — read those first if you are changing the
contract rather than implementing against it.

Two exist today and they are the two worked examples:

| Type | Site | What runs where |
|---|---|---|
| `comfyui` | remote | ComfyUI on the rented box; operator opens a browser at the local port |
| `whisper` | local | a transcription server on the box; the operator's own app stays here |

---

## 1. Pick a site first

Everything else follows from this one answer, so get it right before writing code.

```go
claw.SiteRemote  // the application runs on the rented box
claw.SiteLocal   // it runs on the operator's machine; only inference is rented
```

|  | **remote** | **local** |
|---|---|---|
| On the box | the application itself | an inference server |
| Operator reaches it | browser at the fixed local port | their own app, configured once |
| Extra interface | `claw.Remote` → `Collect` | `claw.Local` → `Clients` |
| Before destroy | **collect outputs** — they exist nowhere else | **revert wiring** — before the endpoint dies |
| `larri down` guard | refuses until collected | destroys freely |

That last row is persisted. `Rig.ClawSite` is written at mint time, before the create
call, because the process that knew is gone by the time a `larri down` in another
terminal has to decide whether destroying the machine destroys the only copy of
something. **An unrecorded site reads as remote**, so a new claw is protected before you
have remembered it exists.

Neither pre-destroy step may block the teardown. Files left behind are lost once; a rig
left alive bills until somebody notices.

---

## 2. The five files

Using `sizzle` as a stand-in name:

```
internal/claw/sizzle/kind.go     the claw.Kind — this document
internal/claw/sizzle/server.go   the runtime.Workload that runs on the box
internal/claw/sizzle/client.go   talking to it, if it needs talking to
examples/sizzle/job.yml          a job file that works
cmd/larri/claw.go                one blank import line
```

Register in the package's `init`, and blank-import it from `cmd/larri/claw.go` beside
the two already there:

```go
const Type claw.Type = "sizzle"

func init() {
	claw.Register(Type, func() claw.Kind { return &Kind{} })
}
```

A **factory**, not a singleton — the registry takes `func() claw.Kind` deliberately. A
`Kind` carries state between its own methods: `Plan` reads the config and resolves what
it found, and `Server` and `Collect` use it. A shared instance would let two concurrent
claws overwrite each other's plan, and the symptom would be a rig serving the wrong
thing rather than an error.

Duplicate registration panics. Two kinds under one name would make which one runs depend
on import order, and the wrong one spends money.

### The daemon must not import your package

`internal/lint` fails the build if `internal/daemon` imports an application package.
This is not style. The coupling it prevents decays silently — reaching in for "just one
thing" compiles and works, and then the next application needs the same exception. If
you find yourself wanting it, the contract is missing something; add it to
`internal/claw` instead.

---

## 3. The job file

`larri claw --config job.yml`. The type owns everything below `type:` and decodes it
itself:

```yaml
type: sizzle
graph: ./g.json        # relative to THIS file, not the cwd
precision: fp16
```

There is no `--graph` flag and there will not be one. A generic command with a flag per
application is not a generic command; it would grow a `--workflow` the day something
wanted one. (This is why `larri claw --workflow w.json` fails: the flag moved into the
file when the claw layer replaced `larri comfy`.)

Decode inside `Plan`:

```go
type Config struct {
	Graph     string `yaml:"graph"`
	Precision string `yaml:"precision"`
}

func (k *Kind) Plan(ctx context.Context, cfg *claw.Config, opt claw.Options) (*claw.Plan, error) {
	if err := cfg.Decode(&k.cfg); err != nil {
		return nil, err
	}
	path := cfg.Resolve(k.cfg.Graph) // against the config's directory
	…
}
```

Two properties you get for free and should not undo:

- **Unknown fields are rejected.** A mistyped key in a file that decides what hardware to
  rent must not be a setting that silently does not apply.
- **`cfg.Resolve` makes relative paths resolve against the job file's own directory.** A
  job file that only works from one cwd is one that breaks in CI. Always route paths
  through it.

---

## 4. `Plan` — everything that can happen before the money

```go
Plan(ctx context.Context, cfg *claw.Config, opt claw.Options) (*claw.Plan, error)
```

This is the most important method you will write, and the rule governing it is §4a:
**a precondition that can be established without renting must be.** The alternative is
paying to discover it, which this project has done often enough to make it an invariant
rather than a preference.

`Plan` must not spend. It may make network calls to *read* things — the Hugging Face API,
a container registry — but it rents nothing. Everything it can already establish is
wrong, it fails on here, for free.

What to check before returning:

- the config parses, and every file it names exists and is readable;
- the model or asset exists, and the operator's token can read it if gated;
- **the image's hardware floors**, read from the image, not from the engine's docs. vLLM's
  matrix says compute capability 7.0; `vllm/vllm-openai` ships no Volta kernels and needs
  CUDA 13.0. That gap rented three V100 boxes that could never have loaded anything;
- weight sizes **measured, not estimated** — see §5;
- format safety: refuse pickle checkpoints, require `safetensors`.

The corollary, and it matters as much: **a check that cannot run proves nothing.** An
unreported CUDA version, an unmeasurable cache, a probe with no `curl` and no Python — all
pass. Failing closed on a missing field empties the market.

### What you return

```go
type Plan struct {
	Criteria       core.Criteria    // what to rent
	Model          core.ModelSpec   // for the journal
	Sizing         *core.SizingPlan // nil = let the standard path size it
	ColdStartBytes uint64           // feeds time-and-cost ranking (§4b)
	Summary        []string         // shown before the confirmation
	Caveats        []string         // weaker guarantees, said out loud
}
```

`Model` is needed even when "the model" is a set of files — the journal persists a
`ModelSpec` for every rig, so give it something meaningful (`ServedName` is what shows
up in status output).

`ColdStartBytes` is what must be downloaded before the claw can work. It is billed at
the rig's hourly rate, which makes the cheapest listing routinely the dearest rig:
$0.109/hr on a 68 Mbps link costs more for any session under six hours than $0.216/hr on
a 1347 Mbps one. Zero ranks on the hourly rate alone, which is usually wrong.

`Caveats` are surfaced at bring-up. A caveat nobody is shown is not a caveat. Whisper's
are a good model to copy — they name what the host can see, and what LARRI will not do
for you.

---

## 5. Sizing: two routes, and how to choose

The question `Plan` answers is `(what I need) → VRAM`, and there are exactly two ways to
answer it.

### Route A — `Sizing: nil`, the standard transformer path

The common case, and the one to prefer. Leave `Sizing` nil and fill in `Model`; the
daemon resolves the model's facts from Hugging Face and runs the KV-cache arithmetic
**per candidate offer**, so shard degree and per-card headroom are accounted for against
the specific hardware being considered. A claw serving an ordinary model should compute
nothing itself.

### Route B — `Sizing: &plan`, a fixed requirement you computed

For a payload with no context length and no attention heads, where the transformer
arithmetic is meaningless. A ComfyUI graph has neither; what it has is a set of files and
a latent area.

Non-nil `Sizing` switches the offer filter to **per-GPU VRAM against a fixed number**.
That is a real constraint, not a convenience: a planned workload executes on one device,
so a requirement met only by summing cards is met nowhere.

`internal/sizing` owns this arithmetic — all of it, for every claw (invariant 5). Do not
put VRAM maths in your claw package. Two helpers exist:

```go
sizing.PlanDiffusion(sizing.DiffusionRequest{
	WeightBytes: total,   // every file the graph loads, as published
	Pixels:      w*h*batch,
})

sizing.PlanSpeech(sizing.SpeechRequest{
	WeightBytes:     resident,  // footprint at rest
	FP16WeightBytes: published, // what the working set scales with
	Concurrency:     n,
})
```

Three things those two encode that you will need if you write a third:

**A shortfall does not mean the same thing everywhere.** ComfyUI answers insufficient
VRAM by moving modules back to host RAM and carrying on — the right image, slowly, which
is a bad rental but not a failed one. CTranslate2 does not offload: a card that cannot
hold the model fails at load. So `PlanDiffusion` reports a *target* and `PlanSpeech`
reports a *floor*, and their warnings say "slowly" and "will not start" respectively. Say
which yours is.

**Quantisation shrinks one number and not the other.** CTranslate2 stores int8 weights and
still computes in float16, so int8 halves what sits at rest and leaves the working set
exactly where it was. Scaling activations off the quantised figure is how an int8 rig gets
chosen a card too small and dies on the first long file. Hence two fields, not one.

**Measure the weights; do not estimate them.** `params × bits-per-weight` is two
approximations multiplied, and it errs toward OOM. Unsloth's `UD-IQ1_M` is 3.31 bits per
weight where the name says 1.75 — 69 GB against an estimated 39 GB, which is a four-card
host against one card that dies on load. If a listing is already being fetched, the size
comes free.

### Turning VRAM into criteria

```go
func (k *Kind) criteria(base core.Criteria, plan core.SizingPlan) core.Criteria {
	need := int((plan.RequiredVRAMBytes + sizing.GiB - 1) / sizing.GiB)
	return claw.RaiseCriteria(base, core.Criteria{
		VRAMPerGPUGB: need,
		VRAMTotalGB:  need,
		RAMGB:        …,
		DiskGB:       …,
	})
}
```

**`claw.RaiseCriteria` only ever raises, and that is the whole contract.** An operator who
asked for 80 GB has said something about the hardware they want; a claw needing 12 GB has
not contradicted them. Quietly lowering a floor rents something cheaper than what was
asked for, which is the one direction that cannot be undone after the fact. Never assign
to `opt.Criteria` directly.

Don't forget disk. Whisper asks for `downloaded × 2 + 30 GB` — the image, the download,
and room for a partial file to sit beside its finished self during a resumed fetch.

---

## 6. `Server` — the workload on the box

```go
Server(p *claw.Plan) runtime.Workload
```

For a remote claw this is the application; for a local one it is the inference server the
operator's app will call. Either way it is an ordinary `runtime.Workload` and the
lifecycle below it does not change.

Three rules that are not negotiable:

- **Bind loopback.** Not configurable, no flag, no config key. A routable inference port is
  unauthenticated access to hardware you are paying for — on Vast, on a public IP shared
  with other tenants. The tunnel forwards to the remote loopback, so this costs nothing.
- **`Ready` performs a real round trip.** A TCP connect or a 200 on `/health` is necessary
  and not sufficient (NFR-05). For a runtime that is a completion; for ComfyUI it is a
  rendered image; for whisper it is a transcription of generated noise — not silence,
  which a VAD would skip.
- **Implement `runtime.FailureClassifier` if you can read your own log.** Returning
  `ClassModelFailure` means "do not retry on another host". Without it every readiness
  failure is a host failure, and a payload that died on an import error gets the same
  image rented again — three times, in a live ComfyUI run, six gigabytes of weights each.

Declare a `runtime.Protocol` too. `ProtocolOpenAI` means chat and licenses the wiring to
point clients at you; anything else means no inference client ever will.

---

## 7. Optional interfaces

Implement these only if they apply. Both exist because of the idle clock — see §8.

```go
// claw.Browser — the operator opens this, rather than configuring a client into it.
CountsAsWork() func(*http.Request) bool

// claw.Holder — work that produces no requests still has to hold the clock.
HoldWhileBusy(ctx context.Context, ep claw.LocalEndpoint, h claw.InFlight)
```

`claw.Browser` also switches on the session cookie: neither a navigation nor a WebSocket
handshake can carry an `Authorization` header, so the local listener needs a
cookie-shaped credential, and LARRI prints a one-time link to establish it.

For `claw.Local`, return `wire.ClientWriter`s rather than writing configuration yourself.
The protocol they follow — detect, back up, apply idempotently, record before the change
takes effect, revert exactly on `down`, verify by probe — is already specified (§10.2,
FR-WIRE-04/05). A claw writing its own wiring would be a weaker duplicate of it, and what
that protocol protects is a file belonging to somebody's editor.

---

## 8. How the idle timeout works

**Idle reclamation is on by default and it destroys**: `--idle-timeout 30m
--idle-action destroy`. `destroy` is the only reclamation action — `stop` keeps paying
storage and surrenders the GPU, which is the worst of both.

`larri claw` takes `--idle-timeout` but not `--idle-action`; a claw always uses the
configured action, which defaults to `destroy`.

### The clock

One clock per rig, in `wire.Activity`, on the local proxy. `IdleFor(now)` returns:

```
0                              if anything is in flight
0                              if no operator request has ever been recorded
now - lastOperatorRequest      otherwise
```

Supervision checks it every poll (`internal/daemon/supervise.go`) and acts:

| State | What happens |
|---|---|
| `idle + 2m ≥ timeout`, action `destroy` | warns once: *"destroyed in N unless used"* |
| `idle ≥ timeout`, action `destroy` | returns a `Termination` — actor `policy`, reason `idle-timeout` |
| `idle ≥ timeout`, action `warn` | warns once per idle stretch |
| used again | the warning re-arms |

The termination carries its evidence: `idle_for`, `window`, `last_request`,
`requests_total`, **and `probes_total`**. An automated destruction that can explain
itself is safe; one that cannot is not.

### The clock starts at READY

Seeded at the READY transition, not by the first request. Until that existed, a rig that
came up and was then walked away from had no clock at all and could never be
reclaimed — which is the likeliest way to abandon one, and precisely the case idle
reclamation is for.

### What counts as activity

**Only operator-attributable requests.** Three exclusions, each of which was load-bearing:

1. **LARRI's own probes never count.** They carry `X-Larri-Probe` and are counted
   separately as `probes`. §12 runs a real round trip every 30 s by design; if that reset
   the clock, the timer would reset every interval and never fire — a feature that appears
   to work and protects nothing.
2. **A browser's background chatter does not count**, if the claw says so. This is
   `claw.Browser.CountsAsWork`. Returning nil counts every non-probe request, which is
   right for `/v1`, where a request *is* the work. It is wrong for a surface with a tab
   attached: ComfyUI's frontend reconnects a WebSocket, re-polls its queue and refetches
   thumbnails for as long as the tab is open. ComfyUI's rule is about intent — *a request
   that asks the GPU to do something is work; a request that asks what the GPU has already
   done is not.* Queueing a render counts; watching one does not. Those requests are still
   carried, and counted as `background`, because an exclusion nobody can see is one nobody
   can check.
3. **A request that got a 503 does not count** as having reached the endpoint. It reached
   nothing.

### Long work with no requests

Erring toward *not work* is only safe because of the other half. `claw.Holder` brackets
the in-flight count while the application has outstanding work:

```go
for {
	busy := queueHasWork()
	// EnterInFlight when busy, ExitInFlight when not
	select { case <-ctx.Done(): return; case <-time.After(15 * time.Second): }
}
```

`IdleFor` returns 0 whenever anything is in flight, so a graph that renders for longer
than the timeout is not destroyed halfway through. **A failed check must release the hold,
not extend it** — an unreachable application is a rig in trouble, and supervision is
better placed to decide about that than a helper whose only move is to keep paying.

If you implement one of `Browser`/`Holder`, think hard about whether you need the other.
Narrowing what counts as work without a way to hold the clock open is how a long job gets
destroyed mid-flight.

### When LARRI is not running

All of the above lives in the local process and stops the moment that process does — a
killed terminal, a closed laptop, a lost network. `internal/deadman` arms the host to stop
the runtime and attempt to halt itself, using only powers the host already holds over
itself. It is explicitly *not* given an account-scoped provider key: that key can destroy
every instance on the account, on a machine whose operator is not you and who has root.
Read that package's doc comment before relying on it — on Vast a container cannot stop
its own billing, and this was measured rather than assumed.

---

## 9. Checklist

Contract:

- [ ] `Type()` matches the name it was registered under (`Open` rejects a mismatch)
- [ ] `Site()` is valid, and the matching interface is implemented — `Remote`→`Collect`,
      `Local`→`Clients`
- [ ] `Describe()` is one line, for `larri claw --list`
- [ ] `Plan` spends nothing and refuses early
- [ ] Criteria go through `claw.RaiseCriteria`
- [ ] `ColdStartBytes` is set
- [ ] VRAM maths is in `internal/sizing`, not in your package
- [ ] Workload binds loopback; `Ready` does a real round trip
- [ ] `Collect` returns failures in `Result.Failed`, never as an error that blocks teardown

Hygiene:

- [ ] SPDX header on every file, tests included
- [ ] Errors read `package: subject: problem` — lowercase, no trailing stop
      (`internal/lint` enforces this)
- [ ] Every value interpolated into a host command is shell-quoted, **including the ones
      in log lines** — a workflow filename carrying `$(…)` once reached a root shell on a
      host holding the operator's HF token
- [ ] `examples/<name>/job.yml` exists and works
- [ ] Blank import added to `cmd/larri/claw.go`
- [ ] Requirements spec gains rows; `docs/PROJECT_STATE.md` is **recounted from the spec**,
      never hand-edited

## 10. Verifying

Run on **Linux**, which is what CI uses. `internal/term` needs `syscall.SIGWINCH` and
`internal/state` cannot `fsync` a directory on Windows, so a Windows checkout reports
failures that have nothing to do with your change.

```bash
make check
```

Spending nothing:

```bash
go run ./cmd/larri -- claw --list
go run ./cmd/larri -- claw --config examples/sizzle/job.yml --dry-run
```

`--dry-run` exercises the whole of `Plan` — config, resolution, measurement, sizing,
search and ranking — and stops before the create call. If your claw is wrong, it should
usually be wrong here.

`internal/claw/fake` is a claw for both sites with zero spend. Use it to test lifecycle
behaviour — that revert runs before the destroy, that a failed collection does not block
one — without touching a provider.

A live run is deliberate, env-gated, and tears down in a `defer` that also runs on panic.
Model it on `internal/daemon/e2e_claw_test.go`:

```bash
LARRI_E2E_SPEND=yes go test -tags e2e -run TestE2EClawComfyUI ./internal/daemon/
```

Budget roughly what the two existing claws cost end to end: **$0.0582** for ComfyUI
(RTX 2000 Ada, 6.5 GB of SDXL fetched on the host) and **$0.0105** for whisper
(RTX A4000).
