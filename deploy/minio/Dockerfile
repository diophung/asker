# syntax=docker/dockerfile:1.7
# Community images are no longer published. Build the official source releases
# instead, including mc for the GDPR harness's S3 object assertions.
# https://github.com/minio/minio/releases/tag/RELEASE.2025-10-15T17-29-55Z
# https://github.com/minio/mc/releases/tag/RELEASE.2025-08-13T08-35-41Z
FROM --platform=$BUILDPLATFORM golang:1.26.8-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS builder
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOMAXPROCS=2

ADD --checksum=sha256:45521908307306e925c98d629e1c17d78c8b72b6ee242b1bfb1409f7d8ee5841 https://codeload.github.com/minio/minio/tar.gz/9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a /tmp/minio.tar.gz
WORKDIR /src/minio
RUN tar -xzf /tmp/minio.tar.gz --strip-components=1 && rm /tmp/minio.tar.gz
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -p 2 -trimpath -buildvcs=false \
    -ldflags='-s -w -X github.com/minio/minio/cmd.Version=2025-10-15T17:29:55Z -X github.com/minio/minio/cmd.ReleaseTag=RELEASE.2025-10-15T17-29-55Z -X github.com/minio/minio/cmd.CommitID=9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a -X github.com/minio/minio/cmd.ShortCommitID=9e49d5e7a648 -X github.com/minio/minio/cmd.CopyrightYear=2025' \
    -o /out/minio .

ADD --checksum=sha256:95cd293c7119f16921a6dc515a1fb74a2227f19fd994b9c8b770a154e802ac44 https://codeload.github.com/minio/mc/tar.gz/7394ce0dd2a80935aded936b09fa12cbb3cb8096 /tmp/mc.tar.gz
WORKDIR /src/mc
RUN tar -xzf /tmp/mc.tar.gz --strip-components=1 && rm /tmp/mc.tar.gz
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -p 2 -trimpath -buildvcs=false \
    -ldflags='-s -w -X github.com/minio/mc/cmd.Version=2025-08-13T08:35:41Z -X github.com/minio/mc/cmd.ReleaseTag=RELEASE.2025-08-13T08-35-41Z -X github.com/minio/mc/cmd.CommitID=7394ce0dd2a80935aded936b09fa12cbb3cb8096 -X github.com/minio/mc/cmd.ShortCommitID=7394ce0dd2a8 -X github.com/minio/mc/cmd.CopyrightYear=2025' \
    -o /out/mc .

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
LABEL org.opencontainers.image.title="Asker MinIO source build" \
      org.opencontainers.image.version="RELEASE.2025-10-15T17-29-55Z" \
      org.opencontainers.image.source="https://github.com/minio/minio" \
      org.opencontainers.image.revision="9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
COPY --from=builder /out/minio /out/mc /usr/bin/
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /src/minio/LICENSE /src/minio/NOTICE /usr/share/licenses/minio/
COPY --from=builder /src/mc/LICENSE /src/mc/NOTICE /usr/share/licenses/mc/
# mc remains usable under the chart's runAsUser=1000 security context.
ENV MINIO_UPDATE=off MC_CONFIG_DIR=/tmp/.mc
RUN mkdir -p /data && chown 1000:1000 /data
EXPOSE 9000 9001
ENTRYPOINT ["minio"]
CMD ["server", "/data"]
