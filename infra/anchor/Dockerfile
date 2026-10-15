# Anchor machine image: varde-agent built from this checkout (same pattern
# as infra/cp). Managed anchor machines are created by the reconciler with
# this image, a volume mounted at /var/lib/varde, and a VARDE_BOOTSTRAP_URL
# the entrypoint fetches its one-use enroll token from.
#
# Build/push from the repo root:
#   docker build -t registry.fly.io/varde-anchors:latest -f infra/anchor/Dockerfile .
#   docker push registry.fly.io/varde-anchors:latest   # after `fly auth docker`
FROM mirror.gcr.io/library/rust:1-bookworm AS build
WORKDIR /src
COPY . .
RUN cargo build --release -p varde-agent

FROM mirror.gcr.io/library/debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/release/varde-agent /usr/local/bin/varde-agent
COPY --chmod=0755 infra/anchor/anchor-entry /usr/local/bin/anchor-entry
ENTRYPOINT ["/usr/local/bin/anchor-entry"]
