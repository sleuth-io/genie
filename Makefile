.PHONY: help build build-darwin build-darwin-amd64 install test eval lint format clean release run dev deps tidy verify update-deps init prepush postpull

# Default target
help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

# Build variables
BINARY_NAME=genie
MAIN_PATH=./cmd/genie
BUILD_DIR=./dist
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT?=$(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE?=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS=-ldflags "-X github.com/sleuth-io/genie/internal/buildinfo.Version=$(VERSION) -X github.com/sleuth-io/genie/internal/buildinfo.Commit=$(COMMIT) -X github.com/sleuth-io/genie/internal/buildinfo.Date=$(DATE)"

build: ## Build the binary
	@echo "Building $(BINARY_NAME)..."
	@go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN_PATH)
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)"

build-darwin: ## Build for macOS (arm64)
	@echo "Building $(BINARY_NAME) for macOS (arm64)..."
	@GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 $(MAIN_PATH)
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64"

build-darwin-amd64: ## Build for macOS (amd64/Intel)
	@echo "Building $(BINARY_NAME) for macOS (amd64)..."
	@GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 $(MAIN_PATH)
	@echo "Built: $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64"

install: build ## Install binary to ~/.local/bin
	@echo "Installing $(BINARY_NAME)..."
	@mkdir -p $(HOME)/.local/bin
	@rm -f $(HOME)/.local/bin/$(BINARY_NAME) && cp $(BUILD_DIR)/$(BINARY_NAME) $(HOME)/.local/bin/
	@echo "✓ $(BINARY_NAME) installed to $(HOME)/.local/bin/$(BINARY_NAME)"
	@case ":$$PATH:" in \
		*":$$HOME/.local/bin:"*) ;; \
		*) echo ""; \
		   echo "⚠ Warning: $$HOME/.local/bin is not in your PATH"; \
		   echo "Add this to your ~/.bashrc or ~/.zshrc:"; \
		   echo "  export PATH=\"\$$PATH:\$$HOME/.local/bin\"" ;; \
	esac

test: ## Run unit tests (no external API calls)
	@echo "Running tests..."
	@OUTPUT=$$(go test -race -cover ./internal/config/... ./internal/engine/... ./internal/llm/... ./pkg/... 2>&1 | grep -v 'no such tool "covdata"'); \
	if echo "$$OUTPUT" | grep -q "^FAIL"; then \
		echo "$$OUTPUT"; \
		exit 1; \
	else \
		PASSED=$$(echo "$$OUTPUT" | grep -c "^ok"); \
		echo "✓ All $$PASSED packages passed"; \
	fi

eval: build ## Run the curated eval set (requires ANTHROPIC_API_KEY + GITHUB_PERSONAL_ACCESS_TOKEN)
	@$(BUILD_DIR)/$(BINARY_NAME) eval --cold --replay

lint: ## Run linters
	@echo "Running linters..."
	@go tool golangci-lint run

format: ## Format code
	@echo "Formatting code..."
	@gofmt -s -w .
	@go mod tidy

clean: ## Clean build artifacts
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	@go clean

release: ## Create release with goreleaser (requires goreleaser)
	@echo "Creating release..."
	@which goreleaser > /dev/null || (echo "goreleaser not found. Install from https://goreleaser.com/install/" && exit 1)
	@goreleaser release --clean

# Development targets
genie: build ## Build and run genie (usage: make genie -- query "{...}")
	@$(BUILD_DIR)/$(BINARY_NAME) $(filter-out $@,$(MAKECMDGOALS))

# Catch-all target to allow passing args to genie (eg: make genie query)
%:
	@:

run: build ## Build and run the binary
	@$(BUILD_DIR)/$(BINARY_NAME)

# Module management
deps: ## Download dependencies
	@echo "Downloading dependencies..."
	@go mod download

tidy: ## Tidy go.mod
	@echo "Tidying go.mod..."
	@go mod tidy

verify: ## Verify dependencies
	@echo "Verifying dependencies..."
	@go mod verify

update-deps: ## Update all dependencies to latest versions
	@echo "Updating all dependencies..."
	@go get -u ./...
	@go mod tidy

init: ## Initialize development environment (download deps)
	@echo "Initializing development environment..."
	@echo "Downloading dependencies..."
	@go mod download
	@echo ""
	@echo "✓ Development environment initialized"

prepush: format lint test build ## Run before pushing (format, lint, test, build)

postpull: init ## Run after pulling (download dependencies)
