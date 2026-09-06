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

# bench starts the mocker, runs both legs (direct and through the gateway) and
# prints the overhead delta. Implemented in M2; the driver does not exist yet.
bench:
	@echo "bench: not implemented until M2 (cmd/bench, cmd/mocker)"

# profile captures cpu + alloc pprof for a bench run. Implemented in M2.
profile:
	@echo "profile: not implemented until M2"

verify: build test lint check-core bench
