# syntax=docker/dockerfile:1
#
# Multi-stage build → a single self-contained Go binary with the frontend
# embedded. No Python, no runtime static dir, no nginx.
#
#   docker build -t buddy .                    # runnable image
#   docker run --rm -p 8080:8080 buddy         # serve on :8080
#   docker build --target export \             # extract just the binary
#     --output type=local,dest=./bin .         #   -> ./bin/buddy

############################  frontend (Node 26.5.0)  ############################
FROM node:26.5.0-slim AS web
WORKDIR /app
# Node 26 no longer bundles corepack; reinstall it so the pnpm version from
# package.json's "packageManager" field stays the single source of truth.
RUN npm install -g corepack@latest && corepack enable
# Manifests first for layer caching.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY apps/web/package.json ./apps/web/
RUN pnpm install --frozen-lockfile
COPY apps/web ./apps/web
RUN pnpm --filter @buddy/web build      # -> /app/apps/web/dist

#############################  server (Go 1.26.5)  ##############################
FROM golang:1.26.5 AS server
WORKDIR /src
ENV CGO_ENABLED=0 GOWORK=off
# Module cache first.
COPY apps/server/go.mod apps/server/go.sum ./apps/server/
RUN cd apps/server && go mod download
COPY apps/server ./apps/server
# Bring the built frontend into the embed directory, then compile it into the
# binary with the `embed` build tag.
COPY --from=web /app/apps/web/dist ./apps/server/internal/webassets/dist
RUN cd apps/server && \
    go build -tags embed -trimpath -ldflags="-s -w" -o /out/buddy ./cmd/server

##################  export stage: extract the raw binary  ######################
FROM scratch AS export
COPY --from=server /out/buddy /buddy

#########################  runtime image (default)  ############################
# Alpine (not distroless) so the image ships a /bin/sh for `docker exec` /
# `kubectl exec` debugging. The Go binary is fully static (CGO_ENABLED=0 above),
# so it runs on musl unchanged; we add ca-certificates for outbound TLS (the LLM
# API, a TLS MySQL) and run as a non-root user to keep the prior posture.
FROM alpine:3.23 AS runtime
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 buddy
COPY --from=server /out/buddy /usr/local/bin/buddy
USER buddy
ENV BUDDY_ENV=prod BUDDY_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/buddy"]
