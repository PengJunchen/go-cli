.PHONY: build test test-cov test-cov-check test-e2e test-mock test-no-plugin \
	test-log test-leak lint fmt vet tidy clean check verify scan help

PROJECT_NAME ?= go-cli
BUILD_DIR    ?= ./bin
COVERAGE_DIR ?= ./coverage
COVERAGE_THRESHOLD ?= 65

GO      ?= go
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w
override VERIFY_TEST_GATE = go run ./internal/verify/cmd/test-gate

# Build
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(PROJECT_NAME) ./cmd/...

# Run all tests
test:
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-count=1 -package=./...

# Run tests with coverage
test-cov:
	@mkdir -p $(COVERAGE_DIR)
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-coverprofile=$(COVERAGE_DIR)/coverage.out -go-arg=-covermode=atomic -package=./...
	go tool cover -func=$(COVERAGE_DIR)/coverage.out

# Coverage threshold check
test-cov-check:
	@mkdir -p $(COVERAGE_DIR)
	@$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-coverprofile=$(COVERAGE_DIR)/coverage.out -package=./...
	@if report=$$(go tool cover -func=$(COVERAGE_DIR)/coverage.out 2>&1); then \
	    :; \
	else \
	    cover_status=$$?; \
	    printf '%s\n' "$$report"; \
	    echo "ERROR: go tool cover failed with exit code $$cover_status"; \
	    exit "$$cover_status"; \
	fi; \
	printf '%s\n' "$$report"; \
	threshold='$(COVERAGE_THRESHOLD)'; \
	if ! awk -v threshold="$$threshold" 'BEGIN { if (threshold !~ /^[0-9]+([.][0-9]+)?$$/ || threshold + 0 > 100) exit 1; exit 0 }'; then \
	    echo "ERROR: Coverage threshold must be a number between 0 and 100"; \
	    exit 2; \
	fi; \
	coverage=$$(printf '%s\n' "$$report" | awk '$$1 == "total:" { count++; if (NF != 3 || $$2 != "(statements)" || $$3 !~ /^[0-9]+([.][0-9]+)?%$$/) invalid = 1; value = $$3; sub(/%$$/, "", value); if (value + 0 > 100) invalid = 1 } END { if (count != 1 || invalid) exit 1; print value }'); \
	if [ $$? -ne 0 ]; then \
	    echo "ERROR: go tool cover output must contain exactly one valid total coverage line"; \
	    exit 1; \
	fi; \
	echo "Total coverage: $${coverage}%"; \
	if awk -v coverage="$$coverage" -v threshold="$$threshold" 'BEGIN { exit !(coverage + 0 >= threshold + 0) }'; then \
	echo "Coverage check passed: $${coverage}% >= $${threshold}%"; \
	else \
	    echo "ERROR: Coverage $${coverage}% is below threshold $${threshold}%"; \
	    exit 1; \
	fi

# Run log-based verification tests
test-log:
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-count=1 -go-arg=-run=TestLogCapturer -package=./internal/verify/...

# Run goroutine leak tests
test-leak:
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-count=1 '-go-arg=-run=TestAssertNoGoroutineLeak|TestGoLeakChecker' -package=./internal/verify/...

# Run E2E-tagged tests in the packages that define them.
test-e2e:
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-count=1 -go-arg=-tags=e2e \
		-package=./internal/core -package=./internal/llm -package=./internal/memory \
		-package=./internal/tools -package=./internal/tui -package=./tests/...

# Run mock-tagged tests only in their supported package scope.
test-mock:
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-count=1 -go-arg=-tags=mock -package=./internal/mock/...

# Exercise the no_plugin stubs in the packages that provide them. Keep this
# separate from Linux jobs that require the native plugin lifecycle test.
test-no-plugin:
	$(VERIFY_TEST_GATE) -go-command $(GO) -go-arg=-race -go-arg=-count=1 -go-arg=-tags=no_plugin \
		-package=./internal/extension/... -package=./internal/cli/...

# AST scan for mock/hardcoded bypass detection
scan:
	$(GO) run ./internal/verify/cmd/scanner-gate \
		-dir ./internal \
		-scanner-command $(GO) \
		-scanner-arg=run \
		-scanner-arg=./internal/verify/cmd/scanner \
		-scanner-arg=-dir \
		-scanner-arg=./internal \
		-scanner-arg=-format \
		-scanner-arg=json

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
