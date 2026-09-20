GO ?= go

.PHONY: build test lint fmt check-core bench profile verify

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

lint:
	golangci-lint run

fmt:
	$(GO) fmt ./...

# Architectural invariant #1: core/ is a library and must stay transport-free.
check-core:
	@if $(GO) list -deps ./core/... | grep -qE '^(net/http|github.com/valyala/fasthttp)'; then \
		echo "FAIL: core/ leaked a transport dependency"; exit 1; \
	else echo "OK: core is transport-free"; fi

# bench starts the mocker, runs every available leg and prints the overhead
# delta. The gateway leg is included automatically once cmd/gateway builds.
# Every knob is an environment variable: make bench RPS=2000 DURATION=10s
bench:
	@bash scripts/bench.sh

# profile captures cpu + alloc pprof of the load driver. From M3 the gateway's
# own profile is the interesting one; this is the driver's, which is how you
# confirm a reported number is the server's cost and not the driver's.
profile:
	@BENCH_FLAGS="-cpuprofile bench-out/cpu.pprof -memprofile bench-out/mem.pprof" \
		RUNS=1 bash scripts/bench.sh
	@echo "profiles: bench-out/cpu.pprof bench-out/mem.pprof"
	@echo "inspect:  go tool pprof -http=: bench-out/cpu.pprof"

verify: build test lint check-core bench
