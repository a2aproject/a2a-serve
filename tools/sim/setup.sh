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
# Prepares a user-simulation run: builds a2a-serve from a git ref, installs
# the a2a CLI and writes env.sh for the simulated users. See tools/sim/SKILL.md.

set -euo pipefail

SIM_TOOLS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SIM_TOOLS/../.." && pwd)"

usage() {
  cat <<'EOF'
usage: tools/sim/setup.sh [flags]

  --ref REF     git ref to build (default origin/main, fetched first); "checkout" builds
                this checkout with its uncommitted changes
  --root DIR    work directory of the run (default $SIM_ROOT or /tmp/a2a-sim)
  --smoke       start one agent, send one message and stop it

Environment:
  SIM_SECRET    name of the variable that holds the model key (default OPENROUTER_API_KEY);
                the variable must be set
  SIM_MODEL     --model of the agents (default openrouter/~google/gemini-flash-latest)
  SIM_AGENT     agent to start (default opencode)
EOF
}

die() { echo "setup: $*" >&2; exit 1; }

REF=origin/main
ROOT="${SIM_ROOT:-/tmp/a2a-sim}"
SMOKE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ref) REF="${2:?--ref needs a value}"; shift 2 ;;
    --root) ROOT="${2:?--root needs a value}"; shift 2 ;;
    --smoke) SMOKE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

SIM_SECRET="${SIM_SECRET:-OPENROUTER_API_KEY}"
SIM_MODEL="${SIM_MODEL:-openrouter/~google/gemini-flash-latest}"
SIM_AGENT="${SIM_AGENT:-opencode}"
[[ -n "${!SIM_SECRET:-}" ]] || die "set $SIM_SECRET to the model key, or SIM_SECRET to the name of the variable that holds it"
command -v docker >/dev/null || die "docker is not on PATH"
command -v go >/dev/null || die "go is not on PATH"
command -v jq >/dev/null || die "jq is not on PATH"

mkdir -p "$ROOT/bin" "$ROOT/lanes" "$ROOT/tmp"
chmod 700 "$ROOT"

# Servers and lanes open the default browser, for example in
# `a2a-serve inspect`. The browser outlives the run and keeps its directory
# in use, so the run gets an xdg-open that only prints the URL.
cat > "$ROOT/bin/xdg-open" <<'EOF'
#!/bin/sh
echo "xdg-open: the simulation doesn't open a browser for $*" >&2
exit 1
EOF
chmod +x "$ROOT/bin/xdg-open"

if [[ "$REF" == checkout ]]; then
  SRC="$REPO_ROOT"
  COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
  git -C "$REPO_ROOT" diff --quiet HEAD || COMMIT="$COMMIT-dirty"
else
  [[ "$REF" == origin/* ]] && git -C "$REPO_ROOT" fetch -q origin
  COMMIT="$(git -C "$REPO_ROOT" rev-parse --short "$REF")"
  SRC="$ROOT/src"
  rm -rf "$SRC"
  mkdir -p "$SRC"
  git -C "$REPO_ROOT" archive "$REF" | tar -x -C "$SRC"
fi

echo "building a2a-serve at $COMMIT"
(cd "$SRC" && CGO_ENABLED=0 go build -o "$ROOT/bin/a2a-serve" .)

CLI_VERSION="$(sed -n 's/^const MinVersion = "\(.*\)"/\1/p' "$SRC/internal/a2acli/a2acli.go")"
[[ -n "$CLI_VERSION" ]] || die "can't read a2acli.MinVersion"
echo "installing the a2a CLI $CLI_VERSION"
GOBIN="$ROOT/bin" go install "github.com/a2aproject/a2a-cli@$CLI_VERSION"
mv "$ROOT/bin/a2a-cli" "$ROOT/bin/a2a"

# Simulated users get their own HOME, which hides the Docker context of the
# real one, so pin its endpoint.
DOCKER_ENDPOINT="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)}"

umask 077
printf '%s' "${!SIM_SECRET}" > "$ROOT/secret"

cat > "$ROOT/env.sh" <<EOF
# Usage: source $ROOT/env.sh <lane> <user>
# Gives each simulated user their own HOME under $ROOT/lanes/<lane>/users/<user>.
_lane=\${1:?usage: source env.sh <lane> <user>}
_user=\${2:?usage: source env.sh <lane> <user>}
export SIM_ROOT="$ROOT"
export SIM_LANE="$ROOT/lanes/\$_lane"
export HOME="\$SIM_LANE/users/\$_user"
mkdir -p "\$HOME"
export XDG_CONFIG_HOME="\$HOME/.config" XDG_DATA_HOME="\$HOME/.local/share" XDG_STATE_HOME="\$HOME/.local/state" XDG_CACHE_HOME="\$HOME/.cache"
export PATH="$ROOT/bin:\$PATH"
# A killed server leaves its temporary files behind, some with the model key,
# so they go in the run directory, which cleanup.sh --delete removes.
export TMPDIR="$ROOT/tmp"
${DOCKER_ENDPOINT:+export DOCKER_HOST="$DOCKER_ENDPOINT"}
export SIM_AGENT="$SIM_AGENT" SIM_MODEL="$SIM_MODEL" SIM_SECRET="$SIM_SECRET"
export $SIM_SECRET="\$(cat "$ROOT/secret")"
unset _lane _user

# sim_start [flags]: starts the agent of the current directory with the run's model and key.
sim_start() {
  a2a-serve start "\$SIM_AGENT" --model "\$SIM_MODEL" --secret "\$SIM_SECRET" --detach --quiet "\$@" &&
    a2a-serve doctor --fix >/dev/null
}

# sim_url: the local URL of the agent of the current directory.
sim_url() { a2a-serve status --url=local; }
EOF

cat > "$ROOT/run.md" <<EOF
- ref: $REF
- commit: $COMMIT
- agent: $SIM_AGENT
- model: $SIM_MODEL
- a2a CLI: $CLI_VERSION
- started: $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF

echo "wrote $ROOT/env.sh and $ROOT/run.md"

if [[ "$SMOKE" == 1 ]]; then
  (
    # shellcheck source=/dev/null
    source "$ROOT/env.sh" smoke owner
    "$SIM_TOOLS/newproj.sh" "$SIM_LANE/proj"
    cd "$SIM_LANE/proj"
    sim_start
    trap 'a2a-serve stop >/dev/null' EXIT
    # The history echoes the prompt, so check the state and the artifacts.
    a2a send -o json -a "$(sim_url)" "Reply with only the word PONG." |
      jq -e '.task.status.state == "TASK_STATE_COMPLETED" and any(.task.artifacts[]?.parts[]?.text // ""; test("PONG"))' >/dev/null ||
      die "smoke test: no PONG from the agent; see a2a-serve logs"
    echo "smoke test passed"
  )
fi
