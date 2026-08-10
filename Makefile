BINARY := gateway
BIN_DIR := bin
IMAGE := api-gateway
VERSION ?= dev

.PHONY: build test run vet fmt docker-build docker-run

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/$(BINARY) ./cmd/server

test:
	go test ./...

run:
	go run ./cmd/server

vet:
	go vet ./...

fmt:
	gofmt -w .

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):latest .

docker-run:
	docker run -d --name api-gateway -p 8080:8080 \
	  -v gateway-data:/data \
	  -e GATEWAY_ADMIN_PASSWORD=change-me \
	  -e GATEWAY_ENC_KEY=change-me \
	  $(IMAGE):latest
