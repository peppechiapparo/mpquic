BINARY=bin/mpquic
CMD_PKG=./cmd/mpquic

MGMT_BINARY=bin/mpquic-mgmt
MGMT_CMD_PKG=./cmd/mpquic-mgmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Build riproducibili: lo stesso commit deve dare lo stesso md5 su qualunque
# host, cosi' il confronto client/VPS della checklist di verifica ha senso.
# -trimpath toglie i path assoluti (module cache, dir di build) dal binario;
# CGO_ENABLED=0 toglie il compilatore C di sistema dall'equazione (gcc di
# Debian 12 sul client != gcc di Ubuntu 24.04 sul VPS dava binari diversi a
# parita' di sorgente e di Go). Senza CGO si usa il resolver DNS puro-Go:
# irrilevante qui, la comunicazione coi VPS e' per IP.
GO_REPRO_ENV=CGO_ENABLED=0
GO_REPRO_FLAGS=-trimpath

.PHONY: build build-mgmt build-all verify clean

build:
	mkdir -p bin
	$(GO_REPRO_ENV) go build $(GO_REPRO_FLAGS) -o $(BINARY) $(CMD_PKG)

build-mgmt:
	mkdir -p bin
	$(GO_REPRO_ENV) go build $(GO_REPRO_FLAGS) -ldflags "-X main.version=$(VERSION)" -o $(MGMT_BINARY) $(MGMT_CMD_PKG)

build-all: build build-mgmt

verify:
	go test ./...

clean:
	rm -rf bin
