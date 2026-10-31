# Developer shortcuts. CI runs the same commands.
.PHONY: test lint build

test:
	go test -race ./...

lint:
	go vet ./...
	go tool staticcheck ./...
	go tool exhaustive ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

build:
	CGO_ENABLED=1 go build -o bin/gdelt-lake ./cmd/gdelt-lake
