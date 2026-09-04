# Everything CI runs, runnable locally with the same commands.
#
# `make check` is the one to run before pushing: it is exactly what the
# workflow in .github/workflows/ci.yml does, so a green local run means a green
# CI run.

GOLANGCI_LINT_VERSION := v2.13.2
GOBIN := $(shell go env GOPATH)/bin

.PHONY: check build test test-race cover lint fmt tools integration clean

check: fmt build lint test

build:
	go build ./...

# The default test run is hermetic: it makes no network calls and spends no
# API quota. See `make integration` for the ones that do.
test:
	go test ./...

# The event manager and the Telegram handler share a storage client across
# goroutines, and the event monitor ticks on its own. -race is where that gets
# checked; it needs cgo, so it lives in CI rather than in `check`.
test-race:
	CGO_ENABLED=1 go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1
	@echo "run 'go tool cover -html=coverage.out' for the annotated source"

lint: tools
	$(GOBIN)/golangci-lint run ./...

fmt:
	gofmt -l -w .

tools:
	@test -x "$(GOBIN)/golangci-lint" || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Opt-in: drives the real Gemini API, needs a config.yaml with a key in it, and
# asserts on model behaviour, so it can fail for reasons unrelated to the code.
integration:
	ULTRA_KIEW_INTEGRATION=1 go test ./internal/googlegenai/ -run Integration -v

clean:
	rm -f coverage.out
	go clean ./...
