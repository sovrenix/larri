# LARRI in ten minutes

Rent a GPU, serve a model on it, point your tools at `127.0.0.1`, stop paying.

This guide gets you from nothing to a working endpoint. It costs about **five
cents**. Everything here was run before it was written down.

---

## 1. Get a provider key

You need an account with at least one of them, funded. LARRI never handles the
money; it spends what you have already put there.

```bash
export VASTAI_API_KEY=...     # console.vast.ai → Account → API Keys
export RUNPOD_API_KEY=...     # runpod.io → Settings → API Keys
```

Put them in your shell profile. LARRI reads them from the environment and
writes them nowhere.

## 2. Build

```bash
git clone https://github.com/sovrenix/larri && cd larri
make build
```

One static binary at `bin/larri`, no runtime dependencies.

## 3. See what a model would cost, without spending anything

```bash
bin/larri up --model Qwen/Qwen2.5-1.5B-Instruct --dry-run
```

```
  sizing     Qwen/Qwen2.5-1.5B-Instruct needs ~4.6 GB VRAM
  search     1801 offers satisfy the criteria
  excluded   608 offers: insufficient-vram
  excluded   268 offers: verification-withdrawn
  select     vastai RTX 3060 12GB $0.047/hr (reliability 0.98)
  plan       ready in ~4m0s (2m0s bringup + 2m0s fetching 10.9 GB over a 918 Mbps link)
  plan       cost: $0.00 to reach ready, $0.05 for 1h of use, $0.05 total
```

Read the `plan` lines before anything else. They are the whole decision: what
you will wait, and what you will pay. Note that **the download is billed** —
which is why LARRI ranks on time-to-ready rather than on the hourly rate. The
cheapest listing is regularly the most expensive rig.

`--dry-run` stops there. Nothing has been rented.

## 4. Bring it up

```bash
bin/larri up --model Qwen/Qwen2.5-1.5B-Instruct
```

It asks once before it spends:

```
  rent RTX 3060 12GB at $0.047/hr? [y/N] y
```

Then it rents, installs a throwaway SSH key, pins the host key, checks the
machine can actually reach Hugging Face, launches vLLM, and waits for a real
completion to come back — not a health check, an actual generated token.

```
  ✓ rig 01M0ZXE8… READY   http://127.0.0.1:8000/v1   model: qwen2.5-1.5b-instruct
    vastai RTX 3060 at $0.047/hr
    key: fKLtIo4Osc…

  policy: idle 30m0s → destroy · no budget ceiling
  holding the tunnel — Ctrl-C to tear down and stop paying
```

Leave that terminal open. It is holding the tunnel and supervising the rig.

## 5. Use it

Any OpenAI-compatible client. The endpoint is always the same address
whatever provider or GPU won the ranking — that is the point of the tunnel.

```bash
curl -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"qwen2.5-1.5b-instruct","messages":[{"role":"user","content":"hello"}]}' \
  http://127.0.0.1:8000/v1/chat/completions
```

For a chat app, use `Generic OpenAI` / `OpenAI-compatible` and give it:

| | |
|---|---|
| Base URL | `http://127.0.0.1:8000/v1` |
| API key | the `key:` printed above |
| Model | the `model:` printed above |

If the client runs in Docker, it needs `--network=host`. The endpoint binds
loopback only and `host.docker.internal` will not reach it — that bind is what
stops any page in your browser from spending your money.

## 6. Stop paying

`Ctrl-C` in the terminal holding the rig, or from anywhere:

```bash
bin/larri down
```

It destroys the rig, **re-queries the provider to confirm it is actually
gone**, and reports the total. A 200 from a delete endpoint is a claim;
absence from the inventory is evidence.

```bash
bin/larri orphans          # anything running that local state cannot account for
bin/larri orphans --destroy
```

Run that if a LARRI process ever dies unexpectedly. It is the only thing that
stops an abandoned rig costing money, because a rented container cannot end
its own billing — measured, not assumed.

---

## Three things worth knowing before you rely on it

**The host can read your prompts.** Inference happens in plaintext on a
stranger's GPU, and whoever owns that machine has root. The tunnel protects
your traffic on the way there; it cannot protect it from the far end. Run
`larri privacy` for the full explanation. Send a rented GPU nothing you could
not afford to publish.

**The cheap end of the market is rough.** Hosts fail: no internet, container
runtimes that cannot attach GPUs, machines that drop off mid-pull. LARRI
checks the local port before spending. Once connected, it verifies compute
capability, CUDA version, VRAM across every card, and whether the host can
reach Hugging Face before downloading weights, then falls back when a host
fails anyway.
Expect an attempt or two to be discarded. That is the market, not a
malfunction.

Two flags shift the odds: `--verified-only` rents only hosts the provider has
verified, and `--min-netspeed` floors the download link. Both cost more per
hour and waste less.

**Idle rigs destroy themselves after 30 minutes** by default, and say so.
Change it with `--idle-timeout`, or turn it off and watch your own bill.

---

## Where to go next

- **[Cookbook](https://sovrenix.github.io/larri/cookbook.html)** — copy-paste
  commands per task, with the hardware each one lands on
- `larri config` — save criteria as named profiles so `larri up` needs no flags
- `larri mcp` — hand the whole lifecycle to Claude Code as tools
- `larri tui` — the same thing under a live dashboard
