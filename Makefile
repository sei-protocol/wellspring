# Pinned golangci-lint version. Bump it together with the CI workflow;
# check-lint-pin fails if they diverge.
GOLANGCI_VERSION := 2.15.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION)

.PHONY: verify build test lint fmt check-lint-pin

verify: check-lint-pin lint test build

build:
	go build ./...

test:
	go test -race -count=1 ./...

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

check-lint-pin:
	@grep -q "version: v$(GOLANGCI_VERSION)$$" .github/workflows/ci.yml || \
		{ echo "golangci-lint pin in .github/workflows/ci.yml differs from v$(GOLANGCI_VERSION)"; exit 1; }
