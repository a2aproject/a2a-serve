# Harness image for `a2a-serve start copilot`. The server runs it as the host
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
RUN npm install -g @github/copilot@latest && npm cache clean --force
# Copilot reads its managed settings only from a file that root owns, so
# they are part of the image. They match the policy the server writes for
# bubblewrap.
RUN mkdir -p /etc/github-copilot \
 && echo '{"allowManagedHooksOnly":true,"permissions":{"disableBypassPermissionsMode":"disable","disableAssistedPermissionsMode":true}}' \
  > /etc/github-copilot/managed-settings.json
RUN mkdir -p /home/agent && chmod 1777 /home/agent
ENV HOME=/home/agent
CMD ["copilot", "--acp"]
