GO ?= go

.PHONY: gate fmt-check vet test build test-integration test-e2e test-hooks test-integration-hooks test-e2e-hooks

# gate is the delivery check every task must keep green.
gate: fmt-check vet test build

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race ./...

build:
	$(GO) build -o bin/harness ./cmd/harness

test-integration:
	$(GO) test -race -tags integration ./...

test-e2e:
	$(GO) test -tags e2e ./e2e/...

# The hook-enabled variants build the binary and the tests with -tags harness_testhooks, which
# turns on the HARNESS_TESTHOOKS subprocess hooks (hold or fail a boot step, hold a shutdown
# step, block the exit). The process and crash suite (internal/daemon) runs only here.
test-hooks:
	$(GO) test -race -tags harness_testhooks ./...

test-integration-hooks:
	$(GO) test -race -tags "integration harness_testhooks" ./...

test-e2e-hooks:
	$(GO) test -tags "e2e harness_testhooks" ./e2e/...
