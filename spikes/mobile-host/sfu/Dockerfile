FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETARCH
WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    for i in 1 2 3; do go mod download && break || sleep 5; done

COPY . .

ARG VERSION=1.0.0
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o sfu ./cmd/sfu

FROM alpine:3
RUN apk add --no-cache wget
COPY --from=builder /app/sfu /usr/local/bin/sfu

RUN addgroup -g 1001 -S gryt && adduser -S gryt -u 1001 -G gryt
USER gryt

EXPOSE 5005

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:5005/health || exit 1

ENTRYPOINT ["sfu"]
