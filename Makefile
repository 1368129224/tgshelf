# Go environment variables
GO         := /usr/local/go/bin/go
GOPATH     := $(CURDIR)/.gohome
GOMODCACHE := $(GOPATH)/pkg/mod
GOCACHE    := $(GOPATH)/cache
GOTOOLCHAIN := auto

export GOPATH GOMODCACHE GOCACHE GOTOOLCHAIN

BINARY     := tgshelf
CMD_PATH   := ./cmd/tgshelf

.PHONY: build test vet lint run docker fmt tidy

## build: build the ./tgshelf binary
build:
	$(GO) build -o $(BINARY) $(CMD_PATH)

## test: run all unit tests
test:
	$(GO) test ./...

## vet: run static analysis
vet:
	$(GO) vet ./...

## lint: check the code with golangci-lint (prints a hint and exits non-zero when missing)
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "error: golangci-lint is not installed. Install it from https://golangci-lint.run/usage/install/ and retry." >&2; \
		exit 1; \
	fi
	GOPATH=$(GOPATH) GOMODCACHE=$(GOMODCACHE) GOCACHE=$(GOCACHE) GOTOOLCHAIN=$(GOTOOLCHAIN) golangci-lint run ./...

## run: run the bot directly from source
run:
	$(GO) run $(CMD_PATH)

## docker: build the Docker image
docker:
	docker build -t tgshelf .

## fmt: format all Go code
fmt:
	$(GO) fmt ./...

## tidy: tidy go.mod and go.sum
tidy:
	$(GO) mod tidy
