# database-access-controller: the manager and dbctl in one distroless, static,
# non-root image. Base images are pinned by digest and pulled from AWS's public
# mirror of Docker Hub (no anonymous rate limits). The build cross-compiles
# rather than emulating the target.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
# Cache mounts rather than `go mod download`, which would fetch the tooling too.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/dbctl ./cmd/dbctl

# The distroless `nonroot` variant runs as uid 65532 and carries system CAs.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
LABEL org.opencontainers.image.title="database-access-controller" \
      org.opencontainers.image.description="Provisions roles, schemas and grants inside RDS and Aurora PostgreSQL" \
      org.opencontainers.image.source="https://github.com/blairham/database-access-controller" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
ENTRYPOINT ["/manager"]

# The published image: ships GoReleaser's prebuilt <os>/<arch> binaries, the
# same ones as the release archives.
FROM runtime AS release
ARG TARGETOS TARGETARCH
COPY ${TARGETOS}/${TARGETARCH}/manager /manager
COPY ${TARGETOS}/${TARGETARCH}/dbctl /dbctl

# Default (last, so a bare `docker build .` gets it): built from source.
FROM runtime
COPY --from=build /out/manager /manager
COPY --from=build /out/dbctl /dbctl
