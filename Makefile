.PHONY: help build start stop restart logout

# Variables
BINARY_NAME=claude-monitor-lite
BUILD_FLAGS=-ldflags="-s -w" -trimpath

# Default target
.DEFAULT_GOAL := help

help: ## Show available commands
	@echo "Claude Monitor Lite"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary
	@go build $(BUILD_FLAGS) -o $(BINARY_NAME)
	@echo "✓ Built: ./$(BINARY_NAME)"

start: build ## Start the monitor
	@./$(BINARY_NAME)

stop: ## Stop the monitor
	@./$(BINARY_NAME) stop

restart: stop start ## Restart the monitor

logout: ## Logout and remove all data
	@./$(BINARY_NAME) logout
