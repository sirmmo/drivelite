# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.23-alpine AS build

WORKDIR /src

# No third-party dependencies, so there is nothing to download here; copying
# go.mod first still keeps this layer cached across source edits.
COPY go.mod ./
COPY main.go ./
COPY internal ./internal

# Static binary: the runtime stage has no libc.
ENV CGO_ENABLED=0
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/drivelite .

# ---- runtime ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates wget tzdata \
 && addgroup -g 65532 -S app \
 && adduser -u 65532 -S -G app app \
 && mkdir -p /data /cache \
 && chown app:app /cache

COPY --from=build /out/drivelite /usr/local/bin/drivelite

USER app:app

ENV DRIVELITE_ROOT=/data \
    DRIVELITE_CACHE_DIR=/cache \
    DRIVELITE_ADDR=:8080

EXPOSE 8080
VOLUME ["/data", "/cache"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/drivelite"]
