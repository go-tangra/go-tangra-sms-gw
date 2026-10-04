.PHONY: generate lint test test-ui test-integration build ui-build build-ui-embedded image compose-up format capture-legacy capture-legacy-runtime
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

test-ui:
	cd ui && npm run test:unit

test-integration:
	$(GO) test -race -tags integration -count=1 ./...

build:
	$(GO) build ./...

ui-build:
	cd ui && npm run build

build-ui-embedded: ui-build
	$(GO) build -tags ui ./...

# NODE_AUTH_TOKEN: a GitHub token with read:packages (e.g. $$(gh auth token)).
image:
	DOCKER_BUILDKIT=1 docker build --secret id=npm_token,env=NODE_AUTH_TOKEN --build-arg VCS_REF=$$(git rev-parse HEAD) -t ghcr.io/go-tangra/go-tangra-sms-gw:dev .

compose-up:
	docker compose -f deploy/compose.yaml up -d

capture-legacy:
	python3 scripts/capture_legacy.py

capture-legacy-runtime:
	python3 scripts/capture_legacy_runtime.py
