BIN ?= $(HOME)/go/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/syednabilashraf/dibs/internal/cli.Version=$(VERSION)

.PHONY: build install test vet

build:
	go build -ldflags "$(LDFLAGS)" -o bin/dibs ./cmd/dibs

install: build
	install -m 0755 bin/dibs $(BIN)/dibs

test:
	go test ./...

vet:
	go vet ./...
