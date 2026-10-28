GO ?= go

.PHONY: build test lint fmt up down sim bench

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

SIM_FLAGS ?= -vehicles 80 -rate 150 -peak 3 -minutes 120 -seeds 5 -batch 2s

sim:
	$(GO) run ./cmd/sim $(SIM_FLAGS)

# Regenerates docs/results.md from code: Go microbenchmarks, the default
# strategy comparison, and a fleet-size sweep.
bench:
	@mkdir -p bin docs
	$(GO) test -run '^$$' -bench 'BenchmarkHungarian|BenchmarkGreedy|BenchmarkAStar|BenchmarkDijkstra' -benchtime 2s ./internal/assign ./internal/geo | tee bin/gobench.txt
	$(GO) run ./cmd/sim $(SIM_FLAGS) -sweep 40,60,80,100,120 -gobench bin/gobench.txt -results docs/results.md -json bin/results.json
