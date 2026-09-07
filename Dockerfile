# arcli — multi-stage Docker build.
# Base: debian-bookworm-slim (~80MB) + CGO-free Go binary.
# Distroless / Alpine swap for v1.0 if image-size becomes a priority.

ARG VERSION=dev

FROM golang:1.25-bookworm AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o arcli ./cmd/arcli

FROM debian:bookworm-slim
ARG VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Non-root user — arcli reads/writes config in $HOME so $HOME must be writable.
RUN useradd -m -u 1000 arcli
USER arcli
WORKDIR /home/arcli

COPY --from=builder /build/arcli /usr/local/bin/arcli
RUN arcli --version

ENTRYPOINT ["arcli"]
