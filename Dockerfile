# quackmux gateway image.
#
#   docker build -t quackmux/quackmux:dev --build-arg VERSION=dev .
#
# The build stage runs on the build platform and cross-compiles for the target
# platform, so multi-platform builds need no emulation (BuildKit):
#
#   docker buildx build --platform linux/amd64,linux/arm64 -t ... .
#
# The engine image is built separately from images/engine.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build

ARG VERSION=dev
ARG TARGETARCH
# Optional module proxy (for example a corporate mirror).
ARG GOPROXY

WORKDIR /src

# Module downloads get their own layer, rebuilt only when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/quackmux ./cmd/quackmux
# The licenses of quackmux and of the Go modules linked into the binary.
RUN GOOS=linux GOARCH=$TARGETARCH bash scripts/third-party-licenses.sh /out/licenses ./cmd/quackmux && \
    cp LICENSE NOTICE /out/licenses/

# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev

LABEL org.opencontainers.image.title="quackmux" \
      org.opencontainers.image.description="Multiplexing gateway for the DuckDB Quack protocol" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/quackmux /quackmux
COPY --from=build /out/licenses /licenses

# 9494: Quack endpoint, 9495: admin API, metrics and health checks.
EXPOSE 9494 9495
USER 65532:65532
ENTRYPOINT ["/quackmux"]
CMD ["serve", "-config", "/etc/quackmux/config.yaml"]
