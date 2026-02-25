BINARY=bin/mpquic

.PHONY: build clean

build:
	mkdir -p bin
	go build -o $(BINARY) ./cmd/mpquic

clean:
	rm -rf bin
