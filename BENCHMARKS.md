# Benchmarks

Every milestone re-runs `make bench` and appends a block here. The protocol is
PLAN.md §8: three consecutive runs, median reported, a regression over 20%
investigated before the milestone is marked done.

**Two numbers, never conflated.** *Gateway overhead* is data-plane cost
(p99 through the gateway − p99 direct to the provider). *Routing overhead* is
control-plane cost and is reported on its own line from M11 onward.

## How to reproduce

```sh
make bench                              # protocol defaults: 1000 RPS, 30s, 3 runs
RUNS=3 DURATION=30s STREAM=1 make bench # streaming leg
make profile                            # driver cpu + alloc pprof
```

The mocker runs **unpaced (`-tps=0`)** for every overhead measurement. Pacing
exists so `curl -N` is legible by eye; leaving it on injects one timer wait per
token into both legs and buries the microseconds being measured under the
mocker's own scheduling noise.

## Reading the output

- **latency** — service time, measured from the driver's dispatch to the last
  byte read. This is what the overhead delta is computed from.
- **sched delay** — how late the driver dispatched each request against its own
  fixed schedule. It is the saturation check, not a server measurement: while
  it stays flat the driver is holding the offered rate and the latency numbers
  describe the server. The driver warns and the numbers should be discarded
  once its p99 passes 2ms.
- **driver cost** — allocations made by the load driver itself, not by the
  server under test. Server-side allocation arrives with the gateway in M3.

---

## M2 — Benchmark harness and baseline — 2026-09-20

Baseline only: there is no gateway to measure yet, so this run establishes the
noise floor the M3 overhead number will be read against. The harness already
runs both legs and prints `gateway_overhead_p99`; the gateway leg joins
automatically as soon as `cmd/gateway` builds.

```
Environment:        Apple M3 Pro (11 cores), 18 GB RAM, macOS (Darwin 25.5.0), go1.27.1 darwin/arm64
Mocker:             -latency=50ms -jitter=0 -tokens=200 -tps=0
Load:               1000 RPS, 30s window, 3s warmup, 256 concurrency, 3 runs
```

### Non-streaming

| Metric | Run 1 | Run 2 | Run 3 | Median |
|---|---|---|---|---|
| Direct p50 | 50.364ms | 50.364ms | 50.364ms | **50.364ms** |
| Direct p99 | 51.085ms | 51.708ms | 51.970ms | **51.708ms** |
| Direct p99.9 | 54.526ms | 56.787ms | 57.049ms | — |
| Success rate | 100% | 100% | 100% | 100% |
| Achieved RPS | 1000.0 | 1000.0 | 1000.0 | — |
| Sched delay p99 | 196.5µs | 225.5µs | 231.7µs | — |

```
Direct p99:         51.708 ms
Gateway p99:        n/a (M3)
Gateway overhead:   n/a (M3)      <-- primary metric
Routing overhead:   n/a (M11)     <-- separate
p99 spread:         1.7% across three runs (bar: <10%)  PASS
Memory (RSS):       21.6 MB (mocker)
Allocs/op:          62.3 driver-side (10.2 kB/op); server-side from M3
```

### Streaming (`-stream`, 200 unpaced SSE chunks + usage chunk + `[DONE]`)

| Metric | Run 1 | Run 2 | Run 3 | Median |
|---|---|---|---|---|
| Direct p50 | 50.823ms | 50.823ms | 50.823ms | **50.823ms** |
| Direct p99 | 51.610ms | 51.937ms | 51.970ms | **51.937ms** |
| Direct TTFB p50 | 50.102ms | 50.102ms | 50.102ms | **50.102ms** |
| Direct TTFB p99 | 50.660ms | 50.725ms | 50.627ms | **50.660ms** |
| Success rate | 100% | 100% | 100% | 100% |
| Sched delay p99 | 213.6µs | 206.3µs | 189.1µs | — |

```
p99 spread:         0.7% across three runs (bar: <10%)  PASS
Memory (RSS):       22.8 MB (mocker)
Allocs/op:          69.4 driver-side (10.5 kB/op)
```

Streaming costs ~460µs more at p50 than non-streaming, which is the cost of
reading 203 SSE events instead of one 1.2 kB body. TTFB sits ~260µs under the
mocker's 50ms first-byte sleep at p50 — the sleep starts when the mocker begins
handling the request, the driver's clock starts before the request is written.

### Notes

- **The noise floor is ~600µs at p99, and that is the number to beat.** Median
  p99 moves 1.7% run to run (≈880µs) against a 50ms mocker latency. A gateway
  overhead in the tens of microseconds is *below* this floor and cannot be
  read off a 50ms-latency run: the driver prints an explicit warning when the
  reported overhead is smaller than the measured run-to-run noise. From M3,
  the clean overhead reading is a `LATENCY=0` run, where the mocker answers
  immediately and the whole p99 is transport plus gateway.
- **Stable across invocations, not just across runs.** A fourth independent
  `make verify` produced a median p99 of 51.511ms against the 51.708ms above —
  0.4% apart, so the harness is not measuring the state of one process launch.
- **Connection reuse verified**: 58 established connections and zero TIME_WAIT
  sockets during a 1000 RPS run. Each request is not paying a dial.
- **The driver is not the bottleneck**: scheduling delay holds ~108µs at p50
  and ~200µs at p99 with no growth across the window, against a 2ms warning
  threshold. The ~108µs floor is Go timer wake-up granularity on darwin/arm64,
  applies identically to both legs, and does not enter the latency histogram —
  latency is timed from dispatch, not from the scheduled instant.
- **Coordinated omission is handled by construction**: load is open-loop. The
  driver schedules against absolute time from the run's start and does not wait
  for a response before scheduling the next request, so a slow server produces
  a backlog visible in `sched delay` rather than a silently reduced rate.
- **Failed requests are excluded from the latency histograms.** A connection
  refused in 30µs would otherwise improve the percentiles it has no business
  appearing in. Failures are counted and categorised separately, and the
  success rate is reported next to every leg.
