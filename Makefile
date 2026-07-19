IMAGE ?= polygon-rpc-proxy
TAG   ?= dev
CONTAINER_ENGINE ?=
GOLANGCI_VERSION ?= v2.12.2

.PHONY: help run test lint vet fmt golangci shellcheck scan image tf-validate aws-deploy aws-smoke aws-destroy

## help: list available targets (default)
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'

## run: start the proxy locally (defaults to https://polygon.drpc.org)
run:
	go run ./cmd/proxy

## test: run all tests with the race detector
test:
	go test -race -count=1 ./...

## vet: static analysis
vet:
	go vet ./...

## fmt: format check (fails if any file is not gofmt-clean)
fmt:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

## golangci: run golangci-lint pinned to the version CI uses
golangci:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION) run --timeout=2m

## shellcheck: lint deployment shell scripts
shellcheck:
	shellcheck scripts/*.sh

## lint: local Go quality gate (fmt, vet, pinned golangci-lint, race tests)
lint: fmt vet golangci test

## scan: run the Go vulnerability scanner over code and dependencies
scan:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## image: build the container image with Docker or Podman
image:
	CONTAINER_ENGINE="$(CONTAINER_ENGINE)" ./scripts/build-image.sh "$(IMAGE)" "$(TAG)"

## tf-validate: format-check and validate the Terraform
tf-validate:
	terraform -chdir=terraform fmt -check -recursive
	terraform -chdir=terraform init -backend=false -input=false
	terraform -chdir=terraform validate

## aws-deploy: deploy and verify the AWS stack
aws-deploy:
	./scripts/deploy.sh

## aws-smoke: test the deployed AWS stack
aws-smoke:
	./scripts/smoke-test.sh

## aws-destroy: destroy the AWS stack after confirmation
aws-destroy:
	./scripts/destroy.sh
