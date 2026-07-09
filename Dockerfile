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
RUN corepack enable
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
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=server /out/buddy /usr/local/bin/buddy
ENV BUDDY_ENV=prod BUDDY_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/buddy"]
