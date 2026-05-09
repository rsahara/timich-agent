.DEFAULT_GOAL := help

-include ../../versions.mk

BUILD_DIR ?= build
BINARY := $(BUILD_DIR)/timich-agent
GO_BUILD_FLAGS ?=
CONFIG_PATH ?= .local/agent.json
DATA_DIR ?= .local/state
DOCKER_IMAGE ?= timich-agent:local

.PHONY: help build test init run docker-build docker-run compose-up compose-down

help:
	@echo "timich-agent"
	@echo ""
	@echo "Available targets:"
	@echo "  make build   Build the timich-agent binary"
	@echo "  make test    Run timich-agent Go tests"
	@echo "  make init    Write a starter local config file"
	@echo "  make run     Run the local admin and media APIs"
	@echo "  make docker-build  Build the timich-agent Docker image"
	@echo "  make docker-run    Run timich-agent in Docker for foreground testing"
	@echo "  make compose-up    Start timich-agent with docker compose"
	@echo "  make compose-down  Stop the docker compose timich-agent"

build:
	@mkdir -p $(BUILD_DIR)
	go build $(GO_BUILD_FLAGS) -ldflags "$(TIMICH_AGENT_LDFLAGS)" -o $(BINARY) ./cmd/timich-agent

test:
	go test ./...

init:
	go run ./cmd/timich-agent init -config $(CONFIG_PATH) -data-dir $(DATA_DIR)

run:
	TIMICH_AGENT_CONFIG_PATH=$(CONFIG_PATH) go run ./cmd/timich-agent serve

docker-build:
	docker build -t $(DOCKER_IMAGE) .

docker-run: docker-build
	mkdir -p .local
	docker run --rm --init \
		-p 8081:8081 \
		-p 8082:8082 \
		-v "$(PWD)/.local:/var/lib/timich-agent" \
		$(DOCKER_IMAGE)

compose-up:
	mkdir -p .local
	docker compose up --build -d

compose-down:
	docker compose down
