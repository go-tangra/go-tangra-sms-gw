# syntax=docker/dockerfile:1
# go-tangra-sms-gw (SMS gateway module, go-tangra v4) - standalone image.
# Build context: the repository root.
#
#   DOCKER_BUILDKIT=1 docker build --secret id=npm_token,env=NODE_AUTH_TOKEN \
#     --build-arg APP_VERSION=4.0.0 --build-arg VCS_REF=$(git rev-parse HEAD) -t ghcr.io/go-tangra/go-tangra-sms-gw:dev .
#
# npm_token is a GitHub token with read:packages for @go-tangra/ui on npm.pkg.github.com.
# It is mounted only for the npm ci step and written to a tmpfs, so it never lands in a layer.

FROM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN --mount=type=secret,id=npm_token,required=true \
    --mount=type=tmpfs,target=/run/npmrc \
    --mount=type=cache,target=/root/.npm \
    set -eu; \
    printf '@go-tangra:registry=https://npm.pkg.github.com\n//npm.pkg.github.com/:_authToken=%s\nignore-scripts=true\nfund=false\naudit=false\n' \
      "$(cat /run/secrets/npm_token)" > /run/npmrc/.npmrc; \
    NPM_CONFIG_USERCONFIG=/run/npmrc/.npmrc npm ci --no-audit --no-fund
COPY ui/ ./
# The API types are regenerated from the contract the service embeds.
COPY api/openapi/ /src/api/openapi/
RUN npm run gen:api && npm run build

FROM golang:1.26.8-alpine AS build
RUN apk add --no-cache git ca-certificates
WORKDIR /src
# GOWORK=off: service repositories never use a go.work; dependencies come from published tags.
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false GOWORK=off GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=ui /src/ui/dist ./ui/dist
ARG APP_VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -tags ui -ldflags "-s -w -X main.version=${APP_VERSION}" -o /out/smsgwsvc ./cmd/smsgwsvc && \
    go build -trimpath -ldflags "-s -w" -o /out/smsgw-migrate ./cmd/smsgw-migrate

FROM gcr.io/distroless/static-debian12:nonroot
ARG APP_VERSION=dev
ARG VCS_REF=unknown
LABEL org.opencontainers.image.source="https://github.com/go-tangra/go-tangra-sms-gw" \
      org.opencontainers.image.title="go-tangra-sms-gw" \
      org.opencontainers.image.version="${APP_VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"
COPY --from=build /out/smsgwsvc /out/smsgw-migrate /usr/local/bin/
# Development defaults only; deployments mount their own configuration and
# secrets (the dev key files are not part of the image).
COPY deploy/dev.yaml deploy/policy.yaml /app/deploy/
WORKDIR /app
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/smsgwsvc"]
CMD ["-config", "deploy/dev.yaml"]
