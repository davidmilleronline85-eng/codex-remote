BINARY := codex-remote

.PHONY: build test tidy fmt install-local

build:
	go build -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

tidy:
	go mod tidy

fmt:
	gofmt -w ./cmd ./internal

install-local:
	go install ./cmd/$(BINARY)
