BINARY=bin/mpquic
CMD_PKG=./cmd/mpquic

MGMT_BINARY=bin/mpquic-mgmt
MGMT_CMD_PKG=./cmd/mpquic-mgmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build build-mgmt build-all verify clean

build:
	mkdir -p bin
	go build -o $(BINARY) $(CMD_PKG)

build-mgmt:
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o $(MGMT_BINARY) $(MGMT_CMD_PKG)

build-all: build build-mgmt

verify:
	go test ./...

clean:
	rm -rf bin
