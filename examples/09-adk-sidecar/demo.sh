#!/usr/bin/env bash
# Copyright 2026 The A2A Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Builds a2a-serve-sidecar (CGO_ENABLED=0) and launches the Go ADK Web UI demo.
#
# Usage:
#   GEMINI_API_KEY=... ./examples/09-adk-sidecar/demo.sh [--port 8080]
#   SIDECAR_AGENT=claude ADK_MODEL=sonnet ANTHROPIC_API_KEY=... ./examples/09-adk-sidecar/demo.sh

set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
PORT="${PORT:-8080}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--port)
      PORT="$2"
      shift 2
      ;;
    -h|--help)
      cat <<'EOF'
Usage: ./demo.sh [--port 8080]

Builds a2a-serve-sidecar with CGO_ENABLED=0 and starts the Go ADK Web UI.

Environment variables:
  GEMINI_API_KEY        Gemini API key (default agent: opencode)
  ANTHROPIC_API_KEY     Anthropic API key (for SIDECAR_AGENT=claude or Claude models)
  SIDECAR_AGENT         opencode (default) or claude
  SIDECAR_SANDBOX       docker (default), gvisor, bwrap, or local
  ADK_MODEL             gemini-3.5-flash-lite (default)
  SIDECAR_EGRESS_ALLOW  Comma-separated extra allowed hosts
  PORT                  Web UI port (default: 8080)
EOF
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1 (try --help)" >&2
      exit 1
      ;;
  esac
done

AGENT="${SIDECAR_AGENT:-opencode}"
if [[ "$AGENT" == "claude" ]]; then
  if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    echo "error: ANTHROPIC_API_KEY must be set when SIDECAR_AGENT=claude" >&2
    exit 1
  fi
elif [[ -z "${GEMINI_API_KEY:-}" && -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "error: set GEMINI_API_KEY (or ANTHROPIC_API_KEY for Claude models) before running ./demo.sh" >&2
  exit 1
fi

echo "==> Building a2a-serve-sidecar (CGO_ENABLED=0)..."
mkdir -p "$DEMO_DIR/bin"
(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$DEMO_DIR/bin/a2a-serve-sidecar" ./cmd/a2a-serve-sidecar)

echo "==> Starting Go ADK Web UI at http://localhost:${PORT} (agent: coder, sidecar: ${AGENT}, sandbox: ${SIDECAR_SANDBOX:-docker})"
cd "$DEMO_DIR"
exec go run ./cmd/demo web -port "$PORT" \
  api -webui_address "localhost:${PORT}" \
  webui -api_server_address "http://localhost:${PORT}/api"
