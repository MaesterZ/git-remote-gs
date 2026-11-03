SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY := git-remote-gs
CMD := .
PLATFORMS := linux/amd64 darwin/arm64

# Leaves out the storage client's unused gRPC transport: ~20 MB release binaries instead of ~32 MB
TAGS := disable_grpc_modules
export GOFLAGS += -tags=$(TAGS)

.PHONY: help
## help| Makefile| Prints this help message
help:
	@echo "Usage:"
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s '|' |  sed -e 's/^/ /'

.PHONY: clean
## clean| Go| Remove built binaries
clean:
	go clean
	rm -rf $(BINARY) dist/

.PHONY: build
## build| Go| Build executable binary
build:
	go build -trimpath -o $(BINARY) $(CMD)

.PHONY: release
## release| Go| Build optimized static binary matching the CI release build
release:
	CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o $(BINARY) $(CMD)

.PHONY: cross
## cross| Go| Build static binaries for linux/amd64 and darwin/arm64 into dist/
cross:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "dist/$(BINARY)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '-s -w' \
			-o dist/$(BINARY)-$$os-$$arch $(CMD) || exit 1; \
	done

.PHONY: install
## install| Go| Install the binary into $(go env GOPATH)/bin
install:
	go install -trimpath $(CMD)

.PHONY: test
## test| Go| Run unit and end-to-end tests (no GCP access needed)
test:
	go test ./...

.PHONY: test-race
## test-race| Go| Run tests with the race detector, as CI does
test-race:
	go test -race ./...

.PHONY: test-bucket
## test-bucket| Go| Run the store contract and the round trip on a real bucket: make test-bucket BUCKET=gs://my-bucket
test-bucket:
	@test -n "$(BUCKET)" || { echo "usage: make test-bucket BUCKET=gs://my-bucket[/prefix]"; exit 2; }
	GIT_REMOTE_GS_TEST_BUCKET=$(BUCKET) go test -count=1 -run 'RealBucket' -v ./e2e/ ./internal/store/

.PHONY: vet
## vet| Go| Run go vet
vet:
	go vet ./...

.PHONY: lint
## lint| Go| Run linter
lint:
	golangci-lint run ./...

.PHONY: format
## format| Go| Run formatter
format:
	golangci-lint fmt ./...

.PHONY: tidy
## tidy| Go| Add missing and remove unused modules
tidy:
	go mod tidy

.PHONY: check
## check| Go| Run vet, lint and the race-enabled tests: what CI runs
check: vet lint test-race
