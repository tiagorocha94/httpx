GO := go
GO_PACKAGES := ./...
CGO_ENABLED := $(shell go env CGO_ENABLED)

.PHONY: check fmt fmt-check test vet lint race race-skipped

check: fmt-check test vet lint $(if $(filter 1,$(CGO_ENABLED)),race,race-skipped)

fmt:
	$(GO) fmt $(GO_PACKAGES)

fmt-check:
	$(if $(shell gofmt -l .),$(error Go files need formatting; run 'make fmt'),@echo All Go files are gofmt formatted)

test:
	$(GO) test $(GO_PACKAGES)

vet:
	$(GO) vet $(GO_PACKAGES)

lint:
	golangci-lint run $(GO_PACKAGES)

race:
	$(GO) test -race $(GO_PACKAGES)

race-skipped:
	@echo "Skipping race tests because CGO_ENABLED=$(CGO_ENABLED); see docs/DEVELOPMENT.md."
