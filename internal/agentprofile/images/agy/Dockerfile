# Harness image for `a2a-serve start agy`. The server runs it as the host
# user with the workspace mounted at its host path and HOME=/home/agent.
FROM golang:1.26-bookworm AS a2a
ARG A2A_CLI_VERSION=latest
RUN CGO_ENABLED=0 GOBIN=/out go install -trimpath github.com/a2aproject/a2a-cli@${A2A_CLI_VERSION}

FROM node:22-trixie-slim
COPY --from=a2a /out/a2a-cli /usr/local/bin/a2a
# Many MCP servers of a project's .mcp.json start with uvx.
COPY --from=ghcr.io/astral-sh/uv:0.13 /uv /uvx /usr/local/bin/
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl ripgrep openssh-client python3 \
 && rm -rf /var/lib/apt/lists/*
# Antigravity's installer script also edits shell profiles, so this fetches
# the release it would install and checks the digest of its manifest.
ARG AGY_RELEASES=https://antigravity-cli-auto-updater-974169037036.us-central1.run.app
RUN set -eu \
 && manifest=$(curl -fsSL "$AGY_RELEASES/manifests/linux_$(dpkg --print-architecture).json") \
 && url=$(printf '%s' "$manifest" | python3 -c 'import json, sys; print(json.load(sys.stdin)["url"])') \
 && sha512=$(printf '%s' "$manifest" | python3 -c 'import json, sys; print(json.load(sys.stdin)["sha512"])') \
 && curl -fsSL -o /tmp/agy.tar.gz "$url" \
 && echo "$sha512  /tmp/agy.tar.gz" | sha512sum -c - \
 && tar -xzf /tmp/agy.tar.gz -C /tmp antigravity \
 && install -m 0755 /tmp/antigravity /usr/local/bin/agy \
 && rm /tmp/agy.tar.gz /tmp/antigravity
RUN mkdir -p /home/agent && chmod 1777 /home/agent
ENV HOME=/home/agent
CMD ["agy", "acp", "--disable-telemetry"]
