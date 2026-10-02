# database-controller — the manager and dbctl in one image.
#
# Distroless, static, non-root: no package manager, no libc and no userland,
# so there is nothing for a scanner to flag and nothing to exec but the two
# binaries below. Base images are pinned by digest; Dependabot moves the pins.

# Base images come from AWS's public mirror rather than Docker Hub. Hub imposes
# anonymous pull rate limits that bite in CI, and it is reachable from fewer
# networks than the mirror -- it is blocked outright on some corporate DNS,
# where a build fails with an i/o timeout resolving the manifest. The mirror
# serves the identical official images, down to the digest.
#
# Cross-compiles on the build host rather than emulating the target: the Go
# toolchain targets any GOOS/GOARCH natively, so a multi-platform build runs
# no foreign code.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
# Cache mounts rather than a `go mod download` layer: the module graph includes
# the lint and codegen tooling, and downloading all of it for two binaries is
# wasted time.
# Both binaries ship in one image: dbctl is what an operator reaches for when
# they need to see what the controller would do, and having it already in the
# image means `kubectl exec` is enough.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd/manager && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/dbctl ./cmd/dbctl

# The distroless `nonroot` variant runs as uid 65532 and carries CA
# certificates, which the TLS connection to RDS needs.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS runtime
LABEL org.opencontainers.image.title="database-controller" \
      org.opencontainers.image.description="Provisions roles, schemas and grants inside RDS and Aurora PostgreSQL" \
      org.opencontainers.image.source="https://github.com/blairham/database-controller" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
ENTRYPOINT ["/manager"]

# The published image: GoReleaser's build context holds <os>/<arch>/<binary>
# for the platform being built, so this stage compiles nothing — it ships the
# same binaries as the release archives. See dockers_v2 in .goreleaser.yaml.
FROM runtime AS release
ARG TARGETOS TARGETARCH
COPY ${TARGETOS}/${TARGETARCH}/manager /manager
COPY ${TARGETOS}/${TARGETARCH}/dbctl /dbctl

# Default: built from source (`make docker-build`, CI). Last, so a bare
# `docker build .` gets it.
FROM runtime
COPY --from=build /out/manager /manager
COPY --from=build /out/dbctl /dbctl
