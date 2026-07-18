BINARY := uploadfun
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test test-integration lint fmt vet run clean hooks

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/uploadfun

test:
	go test ./... -race

test-integration:
	go test -tags integration ./internal/transport/... -v

lint:
	golangci-lint run

# Auto-fixes what golangci-lint safely can: gofmt/goimports formatting and
# golines line-wrapping for anything over lll's 100-char limit. Review the
# diff before committing - golines occasionally wraps in a style you'd
# write differently by hand.
fmt:
	golangci-lint run --fix

vet:
	go vet ./...

run: build
	./$(BINARY) $(ARGS)

clean:
	rm -f $(BINARY)

# Enable the tracked git hooks (runs lint/format before each commit).
hooks:
	git config core.hooksPath .githooks
