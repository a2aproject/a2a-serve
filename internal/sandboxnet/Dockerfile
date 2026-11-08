# The a2a-serve image that Docker Desktop runs next to an agent's container
# to relay its sockets to the server, and whose binary the agent's
# container runs as its launcher. Build it from the repository root:
#
#   docker build -f internal/sandboxnet/Dockerfile .
#
# The legacy builder, which Docker without buildx uses, sets no platform
# arguments, so the binary is for the platform the build runs on. buildx
# builds each platform of --platform in its own build.
FROM golang:1.26-bookworm AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/a2aproject/a2a-serve/internal/cli.version=${VERSION}" \
      -o /out/opt/a2a/a2a-serve . && \
    mkdir -p /out/run/a2a-egress && chmod 1777 /out/run/a2a-egress

FROM scratch
COPY --from=build /out/ /
ENTRYPOINT ["/opt/a2a/a2a-serve"]
