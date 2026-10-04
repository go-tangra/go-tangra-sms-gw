.PHONY: generate lint test test-integration build ui-build compose-up format capture-legacy capture-legacy-runtime
GO ?= go

generate:
	cd ui && npm run gen:api

format:
	gofmt -w $$(find internal cmd pkg api tests -name '*.go' 2>/dev/null)

lint:
	$(GO) vet ./...
	cd ui && npm run lint

test:
	$(GO) test -race ./...

test-integration:
	$(GO) test -race -tags integration -count=1 ./tests/integration/... ./internal/repo/...

build:
	$(GO) build ./...

ui-build:
	cd ui && npm run build

compose-up:
	docker compose -f deploy/compose.yaml up -d

capture-legacy:
	python3 scripts/capture_legacy.py

capture-legacy-runtime:
	python3 scripts/capture_legacy_runtime.py
