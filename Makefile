GO ?= go

.PHONY: build test lint fmt up down

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)
	golangci-lint run ./...

fmt:
	gofmt -w .

up:
	docker compose up -d --build

down:
	docker compose down -v
