GO ?= go

.PHONY: build test test-integration lint fmt up down migrate seed sim bench loadtest

build:
	$(GO) build ./...

test:
	$(GO) test -race ./...

# Needs Postgres (and Redis from M5); see docker-compose.yml or TEST_DATABASE_URL.
test-integration:
	$(GO) test -race -p 1 -tags integration ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "gofmt needed" && exit 1)
	golangci-lint run ./...

fmt:
	gofmt -w .

up:
	docker compose up -d --build

down:
	docker compose down -v

DATABASE_URL ?= postgres://dispatch:dispatch@localhost:5432/dispatch?sslmode=disable

migrate:
	DATABASE_URL=$(DATABASE_URL) $(GO) run ./cmd/api migrate

SIM_FLAGS ?= -vehicles 80 -rate 150 -peak 3 -minutes 120 -seeds 5 -batch 2s

sim:
	$(GO) run ./cmd/sim $(SIM_FLAGS)

# Regenerates docs/results.md from code: Go microbenchmarks, the default
# strategy comparison, and a fleet-size sweep.
bench:
	@mkdir -p bin docs
	$(GO) test -run '^$$' -bench 'BenchmarkHungarian|BenchmarkGreedy|BenchmarkAStar|BenchmarkDijkstra' -benchtime 2s ./internal/assign ./internal/geo | tee bin/gobench.txt
	$(GO) run ./cmd/sim $(SIM_FLAGS) -sweep 40,60,80,100,120 -gobench bin/gobench.txt -results docs/results.md -json bin/results.json

# Registers N vehicles against a running API (default 200).
seed:
	./scripts/seed.sh $(or $(N),200)

# Needs a running stack (make up) and k6 installed. Writes docs/loadtest.md.
loadtest:
	./scripts/loadtest/run.sh
