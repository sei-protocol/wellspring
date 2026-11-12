# Pinned golangci-lint version. Bump it together with the CI workflow;
# check-lint-pin fails if they diverge.
GOLANGCI_VERSION := 2.15.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_VERSION)

.PHONY: verify build test e2e lint fmt check-lint-pin chart-lint

verify: check-lint-pin lint test build chart-lint

build:
	go build ./...

test:
	go test -race -count=1 ./...

# Needs WELLSPRING_E2E_RPC_URL and WELLSPRING_E2E_KEY_FILE; see e2e/e2e_test.go.
e2e:
	go test -tags e2e -count=1 -v ./e2e

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GOLANGCI_LINT) fmt ./...

check-lint-pin:
	@grep -q "version: v$(GOLANGCI_VERSION)$$" .github/workflows/ci.yml || \
		{ echo "golangci-lint pin in .github/workflows/ci.yml differs from v$(GOLANGCI_VERSION)"; exit 1; }

CHART_REQUIRED := --set config.rpc-url=http://rpc:8545 --set-string config.chain-id=713715 --set captcha.existingSecret=captcha
CHART_ALL := $(CHART_REQUIRED) --set httpRoute.enabled=true --set 'httpRoute.hostnames[0]=faucet.example.com' \
	--set 'httpRoute.parentRefs[0].name=gateway' --set serviceMonitor.enabled=true

# Lints the chart and renders it with every optional resource on.
chart-lint:
	helm lint chart --strict $(CHART_REQUIRED)
	helm template wellspring chart $(CHART_ALL) > /dev/null
	helm template wellspring chart --set config.rpc-url=http://rpc:8545 --set-string config.chain-id=713715 \
		--set config.captcha-provider=none > /dev/null
