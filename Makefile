GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/sdk/go/bin/go)
SERVICES = ingest simulator scorer discovery api executor alerter

.PHONY: up down build logs migrate test replay replay-db fixtures dev-deps dashboard-dev fmt vet ps

## Start everything (Postgres, Redis, all services, dashboard)
up: wallets.txt
	docker compose up -d --build

down:
	docker compose down

ps:
	docker compose ps

logs:
	docker compose logs -f --tail=100 $(SVC)

## Apply migrations + seed wallets.txt (against DATABASE_URL, default localhost)
migrate:
	$(GO) run ./cmd/migrate

build:
	$(GO) build -o bin/ ./cmd/...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal

## Run the whole pipeline in-process on fixture data (no keys, no Docker)
replay:
	$(GO) run ./cmd/replay -fixture fixtures/replay.jsonl

## Same, but write results to Postgres so the dashboard shows them (wipes the DB!)
replay-db:
	$(GO) run ./cmd/replay -fixture fixtures/replay.jsonl -db -reset

## Regenerate fixtures/replay.jsonl
fixtures:
	$(GO) run ./cmd/genfixtures -out fixtures/replay.jsonl

## Only Postgres + Redis in Docker; run Go services yourself with `go run ./cmd/<svc>`
dev-deps:
	docker compose up -d postgres redis

dashboard-dev:
	cd dashboard && npm install && API_URL=http://localhost:8080 npm run dev

wallets.txt:
	@test -f wallets.txt || (cp wallets.example.txt wallets.txt && echo "created wallets.txt from example")
