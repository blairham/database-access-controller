# Base images come from AWS's public mirror rather than Docker Hub. Hub imposes
# anonymous pull rate limits that bite in CI, and it is reachable from fewer
# networks than the mirror -- it is blocked outright on some corporate DNS,
# where a build fails with an i/o timeout resolving the manifest. The mirror
# serves the identical official images.
FROM public.ecr.aws/docker/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Both binaries ship in one image: dbctl is what an operator reaches
# for when they need to see what the controller would do, and having it already
# in the image means `kubectl exec` is enough.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/manager ./cmd/manager && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/dbctl ./cmd/dbctl

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /manager
COPY --from=build /out/dbctl /dbctl
USER 65532:65532
ENTRYPOINT ["/manager"]
