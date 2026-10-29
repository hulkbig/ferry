FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine@sha256:5cb67f19c5e7994dcb80c074f7cfca8aeeada45bbdca035f73df269cefdcce61 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=local
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG SOURCE=unknown
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X ferry/internal/buildinfo.Version=${VERSION} -X ferry/internal/buildinfo.Commit=${COMMIT} -X ferry/internal/buildinfo.BuildTime=${BUILD_TIME} -X ferry/internal/buildinfo.Source=${SOURCE}" -o /out/ferry ./cmd/ferry \
    && mkdir -p /out/data /out/tmp

FROM scratch
ARG VERSION=local
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG SOURCE=unknown
LABEL org.opencontainers.image.title="Ferry" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_TIME}" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip
COPY --from=build /out/ferry /usr/local/bin/ferry
COPY --from=build --chown=10001:10001 /out/data /data
COPY --from=build --chown=10001:10001 /out/tmp /tmp
COPY deploy/config.container.yaml /etc/ferry/config.yaml
COPY LICENSES /usr/share/licenses/ferry
COPY LICENSE NOTICE /usr/share/licenses/ferry/
COPY docs/third-party-notices.md docs/container-notices.md docs/runtime-dependencies.json docs/toolchain-notices.json /usr/share/licenses/ferry/
USER 10001:10001
EXPOSE 7890 7900
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/ferry", "-healthcheck-url", "http://127.0.0.1:7890/health"]
ENTRYPOINT ["/usr/local/bin/ferry"]
# Keep restore staging and its journal on the same writable volume.
CMD ["-data-dir", "/data/state", "-config", "/etc/ferry/config.yaml"]
