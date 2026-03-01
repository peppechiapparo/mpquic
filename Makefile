BINARY=bin/mpquic
CMD_PKG=./cmd/mpquic

.PHONY: build verify clean

build:
	mkdir -p bin
	go build -o $(BINARY) $(CMD_PKG)

verify:
	go test ./...

clean:
	rm -rf bin
