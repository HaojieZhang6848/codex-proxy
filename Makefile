GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build test vet fmt clean release run

all: build

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

run: build
	$(GO) run ./cmd/codex-proxy run -config config.yaml

# Cross compiles the Linux and Windows amd64 releases into dist/.
release:
	VERSION=$(VERSION) ./build.sh

clean:
	$(GO) clean
	rm -rf dist
