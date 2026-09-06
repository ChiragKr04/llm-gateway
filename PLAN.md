# LLM Gateway + Router — Build Plan

> **For the agent:** This is a staged build spec. Work **one milestone at a time**. After each milestone, run `make verify`, report the benchmark numbers, and **stop for review before starting the next milestone**. Do not skip ahead, do not implement future milestones early, do not add dependencies not listed in the milestone.

---

## 1. Project goal

Build a high-performance LLM gateway in Go (architecturally modelled on Bifrost) with a pluggable quality router on top (modelled on RouteLLM). The gateway sits in front of multiple LLM providers, exposes an OpenAI-compatible API, and routes each request to the cheapest model likely to answer it well.

This is a **learning project built to production quality**. It will not be sold. Correctness, measurability, and clean architecture matter more than feature count.

### The two halves

| Layer | Purpose | Latency budget |
|---|---|---|
| **Data plane** (gateway) | Auth, provider adapters, streaming, failover, metering | p99 overhead < 100µs |
| **Control plane** (router) | Decide which model serves each request | measured and reported **separately** |

These two numbers must never be conflated in any benchmark output.

---

## 2. Non-goals (do NOT build these)

- No web UI, no dashboard, no frontend of any kind.
- No Kubernetes manifests, Helm charts, or cloud deployment config.
- No user signup, billing integration, Stripe, or payment code.
- No multi-tenancy beyond simple API keys.
- No clustering, no distributed state.
- No Docker until explicitly requested in a milestone.
- No database until M9. In-memory structs before that.
- Do not implement MoE, fine-tuning, or model training in Go.

If a milestone seems to need something on this list, stop and ask.

---

## 3. Architectural invariants

These are non-negotiable and enforced by CI:

1. **`core/` must never import `net/http`, `fasthttp`, or any transport package.** The core is a library. HTTP lives only in `transport/`.
2. **All provider-specific formats translate into `core/schemas` canonical types.** No raw provider maps leak past the adapter boundary.
3. **The router sits behind a single interface** (`router.Router`). The gateway must work correctly with a hardcoded/no-op router.
4. **Billing settlement must survive client disconnect.** Use `context.WithoutCancel` for the settle path.
5. **Every milestone re-runs the benchmark.** A regression in p99 overhead must be reported, not silently accepted.

---

## 4. Tech constraints

- Go 1.24+
- Standard library first. Justify every third-party dependency in the commit message.
- Approved deps as needed: `bytedance/sonic` (JSON), `valyala/fasthttp` (transport, M3+), `HdrHistogram/hdrhistogram-go` (bench), `stretchr/testify` (tests), `redis/go-redis` (M8+), `jackc/pgx` (M9+).
- Table-driven tests. Every provider adapter needs golden-file tests for request/response translation.
- `golangci-lint` clean before any milestone is marked done.

---

## 5. Repository layout

```
llm-gateway/
  core/
    schemas/        # canonical Request, Response, Chunk, Usage, Message
    providers/      # Provider interface + one file per provider
    router/         # Router interface + implementations
    plugins/        # Plugin interface + implementations
    gateway.go      # orchestrator
  transport/
    http/           # server, route mounts, SSE writer
  cmd/
    gateway/        # main binary
    mocker/         # fake provider for benchmarking
    bench/          # load driver
  eval/             # Python sidecar (M13 only)
  Makefile
  PLAN.md
```

---

## 6. Milestones

### M0 — Scaffold and canonical schemas

**Build:**
- `go mod init`, directory structure above, `.gitignore`, Makefile skeleton.
- `core/schemas/schemas.go` with `Request`, `Response`, `Message`, `Part`, `ToolCall`, `Chunk`, `Usage`.
- `Message.Content` is `[]Part` (multimodal-capable), not a bare string.
- `Chunk` carries a `Raw []byte` field to allow zero-copy streaming passthrough later.
- `Request` carries an `Extra map[string]any` escape hatch for provider-specific params.

**Acceptance:**
- `go build ./...` passes.
- `make check-core` passes (see §7).
- Schemas can round-trip a multi-turn conversation with a tool call, verified by test.

---

### M1 — Mock provider

**Build `cmd/mocker`:** an HTTP server that impersonates an OpenAI-compatible provider.

Flags: `-port`, `-latency`, `-jitter`, `-tokens`, `-tps`, `-fail-rate`.

Behaviour:
- Sleeps `latency ± jitter` before first byte.
- Non-streaming: returns a well-formed completion with plausible `usage`.
- Streaming: emits `tokens` SSE chunks paced at `tps`, then a final chunk carrying `usage`, then `data: [DONE]`.
- `-fail-rate` returns 429/500 at the given probability (used in M10).

**Acceptance:**
- `curl -N` against the mocker visibly streams token by token.
- Streaming response ends with a usage-bearing chunk followed by `[DONE]`.

---

### M2 — Benchmark harness and baseline

**Build `cmd/bench`:** a load driver.

Flags: `-target`, `-rps`, `-duration`, `-concurrency`, `-stream`.

Behaviour:
- Drives fixed RPS with a goroutine pool.
- Records per-request latency into an HDR histogram.
- Reports p50/p90/p99/p999, success rate, and allocations if available.
- Supports two legs in one run: **direct to mocker** and **through gateway**, so conditions are identical.
- Prints `gateway_overhead_p99 = p99(through) - p99(direct)`.

**Acceptance:**
- Running against the mocker alone produces a stable baseline across three consecutive runs (< 10% variance).
- Baseline numbers written to `BENCHMARKS.md`.

---

### M3 — Thin passthrough gateway (first overhead number)

**Build:**
- `core/providers/provider.go` — the `Provider` interface (`Name`, `Chat`, `Stream`, `Price`).
- `core/providers/openai_compat.go` — adapter pointed at any OpenAI-compatible base URL (used against the mocker).
- `core/gateway.go` — `Gateway.Chat()`. No router, no plugins yet.
- `transport/http` — `POST /v1/chat/completions` (non-streaming only), `GET /v1/models`.
- `cmd/gateway` — wires it together from a static in-memory config.

**Acceptance:**
- OpenAI Python SDK pointed at the gateway gets a valid non-streaming response.
- `make bench` reports a gateway overhead number. Record it in `BENCHMARKS.md`.
- `make check-core` still passes.

---

### M4 — Streaming

**Build:**
- `Gateway.ChatStream()` returning `<-chan *schemas.Chunk`.
- SSE writer in `transport/http` that flushes per chunk.
- Usage accumulation across the stream. For OpenAI-compatible upstreams, inject `stream_options: {"include_usage": true}` if the caller did not.
- Tool-call delta merging by index (arguments arrive as string fragments).
- A `settle` hook that runs even when the client disconnects mid-stream — use `context.WithoutCancel`.

**Acceptance:**
- OpenAI Python SDK with `stream=True` works unmodified.
- Test: client disconnects at chunk 5 of 200; settle still fires with accumulated usage.
- Test: tool-call arguments split across 10 chunks reassemble correctly.
- `make bench -stream` reports streaming overhead separately. Expect it to be worse than M3 — record it and note why.

---

### M5 — Real providers

**Build adapters for:**
1. Ollama (local, free — this is your weak tier for all later routing work)
2. One OpenAI-compatible commercial provider
3. Anthropic (deliberately different wire format — this is where the abstraction gets tested)

Each adapter must handle: system prompt placement, stop sequences, `finish_reason` vocabulary mapping, tool-call format, reasoning/thinking token accounting, and streaming usage extraction.

**Acceptance:**
- Golden-file tests for request and response translation, both directions, for each provider.
- Same canonical request produces correct provider-native payloads for all three.
- Streaming works for all three.
- Re-run benchmark; overhead unchanged.

---

### M6 — Provider-native endpoints

**Build:** mount `/openai/v1/*`, `/anthropic/v1/*` so native SDKs work by changing only the base URL. Each mount translates its native wire format into canonical types on the way in and back out on the way out.

**Acceptance:**
- Anthropic Python SDK with `base_url` pointed at `/anthropic` works unmodified, streaming and non-streaming.
- Cross-routing works: a request arriving at `/anthropic` can be served by an OpenAI-family model and returns valid Anthropic-format output.

---

### M7 — Plugin chain

**Build:**

```go
type Plugin interface {
    Name() string
    PreHook(ctx context.Context, r *schemas.Request) (*schemas.Request, *ShortCircuit, error)
    PostHook(ctx context.Context, resp *schemas.Response, err error) (*schemas.Response, error)
}
```

`ShortCircuit` allows a plugin to return a response without calling any provider.

Then **move existing logging into a plugin** and add an exact-match response cache plugin (in-memory, LRU).

**Acceptance:**
- Cache plugin short-circuits a repeated identical request; verified by test and visible in benchmark.
- Plugin chain order is deterministic and configurable.
- Overhead with zero plugins registered is unchanged from M6.

---

### M8 — Auth, keys, rate limiting

**Build:**
- API keys: prefixed (`sk-lg-...`), stored hashed, never logged.
- Per-key rate limit (token bucket). In-memory first; Redis-backed behind an interface.
- Proper OpenAI-shaped error envelopes with correct HTTP status codes (401, 429, 400, 402, 500).

**Acceptance:**
- Unauthenticated request returns a correctly-shaped 401.
- Rate limit returns 429 with `Retry-After`.
- Raw keys appear nowhere in logs or DB — verified by test.

---

### M9 — Metering and ledger

**Build:**
- Postgres schema: `api_keys`, `ledger_entries`, `requests`.
- Double-entry ledger with **reserve-then-settle**: reserve an estimate pre-flight (402 if insufficient balance), settle actual on completion, release the difference.
- Per-model pricing table including cached-input and reasoning-token rates.
- Settlement must be idempotent and must run off the hot path.

**Acceptance:**
- Test: 1,000 concurrent requests against a balance that allows only 500 — exactly 500 succeed, ledger balances to zero, no negative balance.
- Test: client disconnect mid-stream still settles.
- Reconciliation report comparing metered totals to provider-reported usage.

---

### M10 — Reliability

**Build:** retry with exponential backoff and jitter, per-provider circuit breaker, ordered fallback chain, request hedging behind a flag.

**Acceptance:**
- With mocker `-fail-rate=0.3`, end-to-end success rate stays above 99%.
- Circuit breaker opens after N consecutive failures and half-opens correctly.
- Retries are never double-billed — verified by test.

---

### M11 — Router interface and heuristic router

**Build:**

```go
type Router interface {
    Choose(ctx context.Context, r *schemas.Request, s *Session) (Choice, error)
}
```

Implement `HeuristicRouter`: hard capability gates first (vision, tools, context length), then length and keyword rules.

Log every decision with its input features to a `routing_decisions` table — this becomes training data later.

**Acceptance:**
- Gateway works identically with `NoopRouter` (regression check).
- **Routing overhead reported as a separate line item** in benchmark output.
- Capability gates are provably never violated (property test).

---

### M12 — Embedding router, sticky and cache-aware

**Build:**
- ONNX-exported sentence embedding model loaded in Go, kNN or logistic classifier over it.
- **Sticky routing:** pin a model per session; re-evaluate only on significant complexity change.
- **Prompt-cache-aware routing:** do not reroute mid-conversation when the prefix is unchanged, because switching providers destroys the upstream prompt cache.
- Decision cache keyed by prompt-prefix hash.
- Tunable threshold `tau` exposed in config.

**Acceptance:**
- Routing overhead measured and reported. If it exceeds 1ms, document the mitigation used.
- **Experiment:** run an identical 20-turn conversation with sticky routing off and on; report the cost difference in `BENCHMARKS.md`. This number is a primary deliverable.
- Sweep `tau` across its range and plot the cost-quality curve.

---

### M13 — Shadow evaluation (Python sidecar)

**Build in `eval/` (Python, not Go):**
- Shadow mode in the gateway: on a sampled fraction of requests, serve the router's choice but asynchronously also call the strong model; store both responses.
- Python scorer using an LLM judge to label which response was better.
- Export a labelled dataset suitable for training the M12 classifier.
- Script to retrain and re-export to ONNX.

**Acceptance:**
- Shadow requests add zero latency to the served response.
- After 200 shadow samples, a labelled dataset exists and a retrained classifier can be swapped in.
- Cost-quality curve regenerated from real traffic, not benchmark data.

---

### M14 — Observability

**Build:** OpenTelemetry traces spanning `auth → plugins → router decision → provider call → settle`. Prometheus metrics: request rate, p50/p95/p99 by model, router decision distribution, spend by model, cache hit rate, circuit breaker state.

**Acceptance:**
- A single request produces one complete trace with all spans.
- Instrumentation adds < 5µs to overhead.

---

## 7. Makefile targets (build in M0)

```make
build:        go build ./...
test:         go test -race ./...
lint:         golangci-lint run
check-core:   # fails if core/ imports any HTTP package
bench:        # starts mocker, runs both legs, prints overhead delta
profile:      # cpu + alloc pprof
verify:       build test lint check-core bench
```

`check-core` implementation:

```make
check-core:
	@if go list -deps ./core/... | grep -qE '^(net/http|github.com/valyala/fasthttp)'; then \
		echo "FAIL: core/ leaked a transport dependency"; exit 1; \
	else echo "OK: core is transport-free"; fi
```

---

## 8. Benchmark protocol

Run after every milestone. Append to `BENCHMARKS.md`:

```
## M<n> — <name> — <date>
Environment:        <cpu, ram, go version>
Mocker:             -latency=50ms -tokens=200 -tps=50
Load:               1000 RPS, 30s, non-streaming
Direct p99:         <x> ms
Gateway p99:        <y> ms
Gateway overhead:   <y-x> µs      <-- primary metric
Routing overhead:   <z> µs        <-- separate, from M11 onward
Memory (RSS):       <n> MB
Allocs/op:          <n>
Notes:              <what changed, what regressed and why>
```

Three consecutive runs; report the median. A regression greater than 20% must be investigated and explained before the milestone is marked done.

---

## 9. Working agreement for the agent

- One milestone per session. Stop and report at the end of each.
- Write tests in the same commit as the code they cover.
- If a milestone's acceptance criteria cannot be met, stop and explain — do not partially implement and move on.
- If you believe a design decision in this plan is wrong, say so before implementing rather than silently deviating.
- Prefer the standard library. Every new dependency needs a one-line justification.
- Never commit API keys, `.env` files, or benchmark output containing real prompts.