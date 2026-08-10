BINARY := gateway
BIN_DIR := bin

.PHONY: build test run vet fmt

build:
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/server

test:
	go test ./...

run:
	go run ./cmd/server

vet:
	go vet ./...

fmt:
	gofmt -w .
