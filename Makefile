GO ?= go

.PHONY: all build test race bench vet fmt lint clean serve

all: vet test build

build:
	$(GO) build -trimpath -ldflags="-s -w" -o bin/fliable ./cmd/fliable

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

bench:
	$(GO) test ./engine/ ./expr/ -bench . -benchmem -run XXX

vet:
	$(GO) vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

fmt:
	gofmt -w .

serve: build
	./bin/fliable serve --data ./data --deploy ./examples/processes

clean:
	rm -rf bin data
