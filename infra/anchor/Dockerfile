# Managed cloud-anchor image: varde-agent + varde-mesh + a bootstrap
# entrypoint that fetches a one-use enroll token from the reconciler over
# 6PN (no CP credential lives in machine config). Build from repo root:
#   docker build -t registry.fly.io/varde-anchors:latest -f infra/anchor/Dockerfile .
FROM mirror.gcr.io/library/rust:1-bookworm AS build
WORKDIR /src
COPY . .
RUN cargo build --release -p varde-agent

FROM mirror.gcr.io/library/golang:1-bookworm AS build-go
WORKDIR /src
COPY apps/mesh apps/mesh
# apps/mesh has `replace github.com/grendalaget/varde/go => ../../go`
COPY go go
RUN cd apps/mesh && go build -o /out/varde-mesh .

FROM mirror.gcr.io/library/debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /src/target/release/varde-agent /usr/local/bin/varde-agent
COPY --from=build-go /out/varde-mesh /usr/local/bin/varde-mesh
COPY infra/anchor/anchor-entry /usr/local/bin/anchor-entry
RUN chmod +x /usr/local/bin/anchor-entry
ENTRYPOINT ["/usr/local/bin/anchor-entry"]
