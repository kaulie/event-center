BINARY  := bin/eventd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
SWAG    ?= swag
SWAG_MAIN := cmd/eventd/main.go

.PHONY: all build test race lint fmt fmt-check vet run clean docker swagger swagger-check

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

swagger: ## Regenerate api/swagger.json from the swag annotations in the code
	@command -v $(SWAG) >/dev/null 2>&1 || { \
	  echo "need swag: go install github.com/swaggo/swag/cmd/swag@latest"; exit 1; }
	$(SWAG) init -g $(SWAG_MAIN) -o api --outputTypes json --parseInternal
	@echo "api/swagger.json 已按注解重新生成"

swagger-check: ## Fail when api/swagger.json is out of sync with the annotations
	@$(MAKE) --no-print-directory swagger >/dev/null 2>&1 || { \
	  echo "swagger 生成失败：先跑 make swagger 看具体报错"; exit 1; }
	@git diff --quiet -- api/swagger.json || { \
	  echo "api/swagger.json 与 handler 上的注解不一致；跑 make swagger 后一并提交"; \
	  git --no-pager diff --stat -- api/swagger.json; exit 1; }
	@echo "api/swagger.json 与注解一致"

run: ## Run locally against ./data/eventd.db
	go run ./cmd/eventd

clean:
	rm -rf bin data

docker: ## Build the container image
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t event-center:$(VERSION) .

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-12s %s\n", $$1, $$2}'
