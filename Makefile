# Makefile for LLM Load Balancer project

# Variables
BINARY_NAME=llm-load-balancer
BUILD_DIR=./build
CONFIG_FILE=./internal/config/config.json
GO_FILES=$(shell find . -name '*.go' -not -path "./vendor/*")
VERSION=$(shell git describe --tags --always --dirty)
LDFLAGS=-ldflags "-X main.Version=$(VERSION)"

# Default target executed when no arguments are given to make
.DEFAULT_GOAL := help

.PHONY: all build test clean lint fmt vet run help deploy

# Target: all - Default target to build, test and lint
all: test lint build

# Target: build - Build the binary
build:
	@echo "Building..."
	@mkdir -p $(BUILD_DIR)
	@go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/server
	@echo "Binary built at $(BUILD_DIR)/$(BINARY_NAME)"

# Target: run - Run the application with default config
run: build
	@echo "Running..."
	@$(BUILD_DIR)/$(BINARY_NAME) --config $(CONFIG_FILE)

# Target: test - Run tests with race detection
test:
	@echo "Running tests..."
	@go test ./... -race -v

# Target: clean - Remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)
	@go clean -testcache

# Target: lint - Run linting tools
lint: fmt vet
	@echo "Linting with golangci-lint..."
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found, installing..."; \
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	fi
	@golangci-lint run

# Target: fmt - Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...

# Target: vet - Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...

# Target: deps - Install dependencies
deps:
	@echo "Installing dependencies..."
	@go mod download
	@go mod tidy

# Target: deploy - Deploy to AWS (runs terraform scripts)
deploy:
	@echo "Deploying to AWS..."
	@cd deployments/terraform && terraform init && terraform apply

# Target: plan - Plan AWS deployment
plan:
	@echo "Planning AWS deployment..."
	@cd deployments/terraform && terraform init && terraform plan

# Target: help - Display help information
help:
	@echo "LLM Load Balancer - Makefile Help"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@echo "  all       Build, test and lint the code (default)"
	@echo "  build     Build the binary"
	@echo "  run       Run the application"
	@echo "  test      Run tests with race detection"
	@echo "  clean     Remove build artifacts"
	@echo "  lint      Run linting tools"
	@echo "  fmt       Format code with go fmt"
	@echo "  vet       Run go vet"
	@echo "  deps      Install dependencies"
	@echo "  deploy    Deploy to AWS (runs terraform scripts)"
	@echo "  plan      Plan AWS deployment without applying"
	@echo "  help      Display this help information"
