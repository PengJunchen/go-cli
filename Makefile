.PHONY: build test test-cov test-log test-leak lint fmt vet tidy clean check verify scan help

PROJECT_NAME ?= go-cli
BUILD_DIR    ?= ./bin
COVERAGE_DIR ?= ./coverage
COVERAGE_THRESHOLD ?= 65

GO      ?= go
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w

# Build
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(PROJECT_NAME) ./cmd/...

# Run all tests
test:
	go test -race -count=1 ./internal/... ./tests/...

# Run tests with coverage
test-cov:
	@mkdir -p $(COVERAGE_DIR)
	go test -race -coverprofile=$(COVERAGE_DIR)/coverage.out -covermode=atomic ./internal/... ./tests/...
	go tool cover -func=$(COVERAGE_DIR)/coverage.out

# Coverage threshold check
test-cov-check:
	@mkdir -p $(COVERAGE_DIR)
	@go test -coverprofile=$(COVERAGE_DIR)/coverage.out ./... 2>/dev/null
	@COVERAGE=$$(go tool cover -func=$(COVERAGE_DIR)/coverage.out | grep total | awk '{print $$3}' | sed 's/%//') && \
		echo "Total coverage: $${COVERAGE}%" && \
		if [ $$(echo "$$COVERAGE < $(COVERAGE_THRESHOLD)" | bc) -eq 1 ]; then \
			echo "ERROR: Coverage $${COVERAGE}% is below threshold $(COVERAGE_THRESHOLD)%"; \
			exit 1; \
		else \
			echo "Coverage check passed: $${COVERAGE}% >= $(COVERAGE_THRESHOLD)%"; \
		fi

# Run log-based verification tests
test-log:
	go test -race -count=1 -run "TestLogCapturer" ./internal/verify/...

# Run goroutine leak tests
test-leak:
	go test -race -count=1 -run "TestAssertNoGoroutineLeak|TestGoLeakChecker" ./internal/verify/...

# AST scan for mock/hardcoded bypass detection
scan:
	go run ./internal/verify/cmd/scanner -dir ./internal -format text

# Run golangci-lint
lint:
	golangci-lint run ./...

# Format code
fmt:
	gofmt -s -w .
	@if command -v goimports >/dev/null 2>&1; then \
		goimports -w . ; \
	else \
		echo "Note: goimports not found, skipping (install with: go install golang.org/x/tools/cmd/goimports@latest)" ; \
	fi

# Run go vet
vet:
	go vet ./...

# Tidy modules
tidy:
	go mod tidy

# Clean build artifacts
clean:
	rm -rf $(BUILD_DIR) $(COVERAGE_DIR)
	rm -f *.out *.test

# Pre-commit check (run all)
check: fmt vet lint build test

# Full verification suite
verify: check scan test-log test-leak
	@echo "=== Verification complete ==="

# Help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
