# syntax=docker/dockerfile:1
#
# Self-contained image for the Varde control plane. Builds the dashboard SPA
# from this checkout, embeds it into the CP binary, and ships a minimal
# runtime image. Build context is the repository root:
#
#   docker build -f infra/cp/Dockerfile .
#   fly deploy --config infra/cp/fly.toml        (also from the repo root)

# --- Stage 1: build the dashboard SPA (apps/web) --------------------------
FROM mirror.gcr.io/library/node:22-slim AS web
RUN npm install -g pnpm@12.9.1
WORKDIR /src
COPY . .
RUN pnpm install --frozen-lockfile \
 && pnpm --dir apps/web build

# --- Stage 2: build the CP binary with the embedded webui -----------------
FROM mirror.gcr.io/library/golang:1.27-bookworm AS cp
COPY --from=web /src /src
WORKDIR /src
# Same recipe as `make webui-dist && go build` (see repo Makefile).
RUN rm -rf apps/controlplane/internal/webui/dist \
 && cp -r apps/web/dist apps/controlplane/internal/webui/dist \
 && cd apps/controlplane \
 && CGO_ENABLED=0 go build -o /out/varde-control-plane .

# --- Runtime ---------------------------------------------------------------
# debian-slim + fuse3 for LiteFS's FUSE mount; runs as root since mounting
# /litefs requires it. litefs mount supervises the CP (see litefs.yml exec).
FROM mirror.gcr.io/library/debian:bookworm-slim
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl fuse3 sqlite3 \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL \
      "https://github.com/superfly/litefs/releases/download/v0.5.14/litefs-v0.5.14-linux-${TARGETARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin litefs
COPY infra/cp/litefs.yml /etc/litefs.yml
COPY --chmod=0755 infra/cp/varde-cp-entry /usr/local/bin/varde-cp-entry
COPY --from=cp /out/varde-control-plane /varde-control-plane
EXPOSE 8080
# Flags are all env-driven: VARDE_LISTEN, VARDE_DB, VARDE_PUBLIC_URL,
# VARDE_SIGNUP, VARDE_PRIMARY_FILE, VARDE_EMBEDDED_RELAY(_ADDR),
# VARDE_LOG_LEVEL (see apps/controlplane/main.go).
ENTRYPOINT ["litefs", "mount"]
