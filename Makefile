BINARY  := drivelite
IMAGE   := drivelite:latest
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help

## help: list the available targets
help:
	@echo "drivelite $(VERSION)"
	@echo
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  make /' | column -t -s ':'

## build: compile the binary for this machine
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

## run: serve ./share on :8080 with password "dev"
run:
	@mkdir -p share cache
	DRIVELITE_ROOT=./share \
	DRIVELITE_CACHE_DIR=./cache \
	DRIVELITE_PASSWORD=dev \
	go run .

## test: run the test suite with the race detector
test:
	go test -race ./...

## cover: run tests and open a coverage report
cover:
	go test -coverprofile=coverage.txt -covermode=atomic ./...
	go tool cover -func=coverage.txt | tail -1
	go tool cover -html=coverage.txt

## fmt: rewrite sources with gofmt
fmt:
	gofmt -w .

## vet: run go vet
vet:
	go vet ./...

## check: everything CI checks (format, vet, tests)
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; \
	fi
	@echo "gofmt ok"
	go vet ./...
	go test -race ./...

## docker: build the container image
docker:
	docker build -t $(IMAGE) .

## docker-run: build and run the image against ./share
docker-run: docker
	@mkdir -p share
	docker run --rm -p 8080:8080 \
		-v "$(PWD)/share:/data:ro" \
		-e DRIVELITE_PASSWORD=dev \
		$(IMAGE)

## clean: remove build artefacts
clean:
	rm -rf $(BINARY) dist build coverage.txt cache

.PHONY: help build run test cover fmt vet check docker docker-run clean
