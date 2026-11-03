# Harness image for `a2a serve claude`. The server runs it as the host
# user with the workspace mounted at its host path and HOME=/home/agent.
# The harness calls other agents with the a2a CLI of this image, so
# A2A_CLI_VERSION must match a2acli.PinnedVersion; the published tag is
# a2a-<A2A_CLI_VERSION>.
FROM golang:1.26-bookworm AS a2a
ARG A2A_CLI_VERSION=v0.3.2-0.20261030153310-f90e4eb628cd
RUN CGO_ENABLED=0 GOBIN=/out go install -trimpath github.com/a2aproject/a2a-cli@${A2A_CLI_VERSION}

FROM node:22-bookworm-slim
COPY --from=a2a /out/a2a-cli /usr/local/bin/a2a
# Many MCP servers of a project's .mcp.json start with uvx.
COPY --from=ghcr.io/astral-sh/uv:0.13 /uv /uvx /usr/local/bin/
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl ripgrep openssh-client python3 \
 && rm -rf /var/lib/apt/lists/*
RUN npm install -g @zed-industries/claude-agent-acp@latest @anthropic-ai/claude-code@latest && npm cache clean --force
RUN mkdir -p /home/agent && chmod 1777 /home/agent
ENV HOME=/home/agent
CMD ["claude-agent-acp"]
