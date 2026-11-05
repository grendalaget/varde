# Build from the repository root; gateway itself terminates TLS.
FROM mirror.gcr.io/library/golang:1-bookworm AS build
WORKDIR /src
COPY apps/gateway apps/gateway
COPY go go
RUN cd apps/gateway && CGO_ENABLED=0 go build -trimpath -o /out/varde-gateway .

FROM mirror.gcr.io/library/debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/varde-gateway /usr/local/bin/varde-gateway
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/varde-gateway"]
