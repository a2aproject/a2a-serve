# The relay image, ghcr.io/a2aproject/a2a-serve-relay: a2a-serve and the
# agentmesh-one it pins, running `a2a-serve relay run` in a relay directory
# that lasts as long as the container. Build it from the repository root:
#
#   docker build -f internal/relay/image/Dockerfile .
#
# The agentmesh-one image is agentmesh.Image, which a test keeps in sync.
FROM ghcr.io/google/agentmesh-one:v0.2.0@sha256:2d462256b7da24c29dd8477343d7032cc1e4e55a97d335117faedf5438666ca3 AS agentmesh

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/a2aproject/a2a-serve/internal/cli.version=${VERSION}" \
    -o /out/a2a-serve . && mkdir -p /out/relay

FROM gcr.io/distroless/static:nonroot
COPY --from=agentmesh /agentmesh-one /agentmesh-one
COPY --from=build /out/a2a-serve /a2a-serve
COPY --from=build --chown=nonroot:nonroot /out/relay /relay
ENV A2A_SERVE_AGENTMESH_ONE=/agentmesh-one
USER nonroot:nonroot
WORKDIR /relay
ENTRYPOINT ["/a2a-serve", "relay", "run", "--dir", "/relay"]
CMD ["--listen", ":8080"]
