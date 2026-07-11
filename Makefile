BINARY := uploadfun
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test test-integration lint vet run clean hooks

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/uploadfun

test:
	go test ./... -race

test-integration:
	go test -tags integration ./internal/transport/... -v

lint:
	golangci-lint run

vet:
	go vet ./...

run: build
	./$(BINARY) $(ARGS)

clean:
	rm -f $(BINARY)

# Enable the tracked git hooks (runs lint/format before each commit).
hooks:
	git config core.hooksPath .githooks
