BINARY  := bin/eventd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build test race lint fmt fmt-check vet run clean docker

all: lint test build

build: ## Build the eventd binary into bin/
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/eventd
	@echo "built $(BINARY) ($(VERSION))"

test: ## Run the full test suite
	go test ./...

race: ## Run tests with the race detector
	go test -race -count=1 ./...

lint: fmt-check vet ## Check formatting and vet

fmt: ## Rewrite files with gofmt
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$out" ]; then echo "gofmt required for:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

run: ## Run locally against ./data/eventd.db
	go run ./cmd/eventd

clean:
	rm -rf bin data

docker: ## Build the container image
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t event-center:$(VERSION) .

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-12s %s\n", $$1, $$2}'
