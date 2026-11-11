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
# Records a demo scenario from tools/demo/scenarios as an asciinema cast.
# See tools/demo/SKILL.md.

set -euo pipefail

DEMO_TOOLS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_TOOLS/../.." && pwd)"

usage() {
  cat <<'EOF'
usage: tools/demo/record.sh <scenario>|list [flags]

  --fake          run fakeacp agents with no sandbox: no Docker or API key (implies --build)
  --build         build a2a-serve from this checkout instead of using the one on PATH
  --out DIR       where to write the recordings (default tools/demo/out)
  --size COLSxROWS terminal size (default 120x40)
  --speed N       type and pause N times faster (default 1)
  --gif           also render a GIF, with the browser videos spliced in (needs agg and ffmpeg)
  --gif-speed N   play the GIF N times faster (default 1.5)
  --keep          keep the work directory with the agent home directories
  --no-record     play the scenario without recording it, as CI does (no asciinema needed)
EOF
}

[[ $# -ge 1 ]] || { usage >&2; exit 2; }
DEMO_NAME="$1"
shift

if [[ "$DEMO_NAME" == list ]]; then
  for f in "$DEMO_TOOLS"/scenarios/*.sh; do
    printf '%-28s %s\n' "$(basename "$f" .sh)" "$(sed -n 's/^# summary: //p' "$f")"
  done
  exit 0
fi

SCENARIO="$DEMO_TOOLS/scenarios/$DEMO_NAME.sh"
[[ -f "$SCENARIO" ]] || { echo "error: no scenario $SCENARIO (try: record.sh list)" >&2; exit 2; }

DEMO_FAKE=""
BUILD=""
DEMO_OUT="$DEMO_TOOLS/out"
SIZE="120x40"
SPEED=1
GIF=""
GIF_SPEED=1.5
KEEP=""
NO_RECORD=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --fake) DEMO_FAKE=1; BUILD=1; shift ;;
    --build) BUILD=1; shift ;;
    --out) DEMO_OUT="$2"; shift 2 ;;
    --size) SIZE="$2"; shift 2 ;;
    --speed) SPEED="$2"; shift 2 ;;
    --gif) GIF=1; shift ;;
    --gif-speed) GIF_SPEED="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    --no-record) NO_RECORD=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) echo "error: unknown flag $1" >&2; usage >&2; exit 2 ;;
  esac
done
DEMO_COLS="${SIZE%x*}"
DEMO_ROWS="${SIZE#*x}"
DEMO_TYPE_DELAY="$(awk -v s="$SPEED" 'BEGIN { print 0.035 / s }')"
DEMO_PAUSE_SCALE="$(awk -v s="$SPEED" 'BEGIN { print 1 / s }')"

if [[ -n "$NO_RECORD" && -n "$GIF" ]]; then
  echo "error: --gif needs a recording; drop --no-record" >&2
  exit 2
fi
tools=(tmux)
[[ -n "$NO_RECORD" ]] || tools+=(asciinema)
for tool in "${tools[@]}"; do
  command -v "$tool" >/dev/null || { echo "error: $tool is not installed" >&2; exit 1; }
done
if [[ -z "$NO_RECORD" ]]; then
  ASCIINEMA_MAJOR="$(asciinema --version | sed -E 's/^asciinema ([0-9]+).*/\1/')"
fi

mkdir -p "$DEMO_OUT"
DEMO_OUT="$(cd "$DEMO_OUT" && pwd)"
CAST="$DEMO_OUT/$DEMO_NAME.cast"
rm -f "$DEMO_OUT/$DEMO_NAME"-browser-*.webm "$DEMO_OUT/$DEMO_NAME.markers"

# A short, stable path: it shows in the recording, and macOS limits Unix
# socket paths to 104 bytes.
DEMO_WORK="${DEMO_WORK:-/tmp/a2a-demo}"
if [[ -e "$DEMO_WORK" ]]; then
  echo "error: $DEMO_WORK exists; stop its agents and remove it, or set DEMO_WORK" >&2
  exit 1
fi
if tmux -L a2a-demo has-session 2>/dev/null; then
  echo "error: a demo is already running (tmux -L a2a-demo attach -r); stop it with tmux -L a2a-demo kill-server" >&2
  exit 1
fi
REAL_HOME="$HOME"
mkdir -p "$DEMO_WORK/you" "$DEMO_WORK/bin"

if [[ -n "$BUILD" ]]; then
  echo "[demo] building a2a-serve" >&2
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -tags agentmesh -o "$DEMO_WORK/bin/a2a-serve" .)
  if [[ -n "$DEMO_FAKE" ]]; then
    (cd "$REPO_ROOT" && go build -o "$DEMO_WORK/bin/fakeacp" ./internal/agentsrv/testdata/fakeacp)
  fi
fi
export PATH="$DEMO_WORK/bin:$PATH"
for tool in a2a a2a-serve; do
  command -v "$tool" >/dev/null || { echo "error: $tool is not on PATH" >&2; exit 1; }
done

# The scenario runs with its own HOME, so it starts with no pairings or
# contacts and leaves the real one alone. Docker keeps its context there.
if [[ -z "${DOCKER_HOST:-}" ]] && command -v docker >/dev/null; then
  DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null || true)"
  [[ -n "$DOCKER_HOST" ]] && export DOCKER_HOST
fi
[[ -d "$REAL_HOME/.docker" ]] && export DOCKER_CONFIG="$REAL_HOME/.docker"
# Docker Desktop keeps its credential helpers, such as docker-credential-desktop,
# next to its CLI. Login shells add them to PATH; agents' shells may not.
for dir in /Applications/Docker.app/Contents/Resources/bin "$REAL_HOME/.docker/bin"; do
  [[ -d "$dir" ]] && PATH="$PATH:$dir"
done
if [[ -z "${PLAYWRIGHT_BROWSERS_PATH:-}" ]]; then
  if [[ "$(uname)" == Darwin ]]; then
    export PLAYWRIGHT_BROWSERS_PATH="$REAL_HOME/Library/Caches/ms-playwright"
  else
    export PLAYWRIGHT_BROWSERS_PATH="$REAL_HOME/.cache/ms-playwright"
  fi
fi
# Git finds credentials, such as the macOS keychain's, through HOME. This
# helper asks the real HOME's helpers, so that a recipe can be fetched from a
# private repository.
cat >"$DEMO_WORK/bin/git-credential-realhome" <<EOF
#!/bin/sh
[ "\$1" = get ] || exit 0
HOME="$REAL_HOME" GIT_TERMINAL_PROMPT=0 exec git credential fill
EOF
chmod 700 "$DEMO_WORK/bin/git-credential-realhome"
if [[ -z "${GIT_CONFIG_COUNT:-}" ]]; then
  export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=realhome
fi
# Go keeps its caches under HOME. Keep the real ones, so that a scenario that
# installs a tool reuses the downloaded modules.
if command -v go >/dev/null; then
  GOPATH="$(go env GOPATH)" GOMODCACHE="$(go env GOMODCACHE)" GOCACHE="$(go env GOCACHE)"
  export GOPATH GOMODCACHE GOCACHE
fi
export DEMO_REAL_HOME="$REAL_HOME" DEMO_PAUSE_SCALE
export HOME="$DEMO_WORK/you"
export BASH_SILENCE_DEPRECATION_WARNING=1
export A2ACLI_TIMEOUT="${A2ACLI_TIMEOUT:-5m}"
unset TMUX

# The agent the examples start. Fake mode swaps in fakeacp.
if [[ -n "$DEMO_FAKE" ]]; then
  AGENT="acp"
  AGENT_FLAGS="--sandbox local --command fakeacp"
else
  AGENT="${DEMO_AGENT:-opencode}"
  AGENT_FLAGS="--model ${DEMO_MODEL:-openrouter/~google/gemini-flash-latest} --secret ${DEMO_SECRET:-OPENROUTER_API_KEY}"
fi

# shellcheck source=lib.sh
source "$DEMO_TOOLS/lib.sh"

cat >"$DEMO_WORK/bashrc" <<EOF
PS1='\[\e[1;34m\]\w\[\e[0m\] \$ '
PS2='> '
HISTFILE=
PROMPT_COMMAND='tmux -L $DEMO_SOCKET wait-for -S "demo-ready-\$DEMO_PANE"'
cd ~
EOF

ASCIINEMA_PID=""
cleanup() {
  local rc=$?
  set +e
  if [[ $rc -ne 0 ]]; then
    for name in $(t list-panes -s -t "$DEMO_SESSION" -F '#{@name}' 2>/dev/null); do
      echo "--- pane $name ---" >&2
      screen "$name" | tail -n 25 >&2 || true
    done
  fi
  [[ -n "${DEMO_BROWSER_PID:-}" ]] && kill "$DEMO_BROWSER_PID" 2>/dev/null
  t kill-server 2>/dev/null
  [[ -n "$ASCIINEMA_PID" ]] && wait "$ASCIINEMA_PID" 2>/dev/null
  find "$HOME" -path '*/.a2a/config.yaml' 2>/dev/null | while read -r cfg; do
    a2a-serve stop --dir "$(dirname "$(dirname "$cfg")")" >/dev/null 2>&1 ||
      echo "[demo] could not stop the agent in $(dirname "$(dirname "$cfg")")" >&2
  done
  declare -F teardown >/dev/null && teardown
  if [[ -n "$KEEP" ]]; then
    echo "[demo] kept $DEMO_WORK" >&2
  else
    chmod -R u+w "$DEMO_WORK" 2>/dev/null
    rm -rf "$DEMO_WORK"
  fi
  exit $rc
}
trap cleanup EXIT

a2a-serve doctor --fix >/dev/null

# shellcheck source=/dev/null
source "$SCENARIO"
if declare -F prepare >/dev/null; then
  log "preparing (off camera)"
  prepare
fi
layout

if [[ -n "$NO_RECORD" ]]; then
  log "playing $DEMO_NAME without recording (watch: tmux -L $DEMO_SOCKET attach -r)"
  scene
  browser_wait
  t kill-server
  ls -1 "$DEMO_OUT/$DEMO_NAME"-browser-*.webm 2>/dev/null || true
  exit 0
fi

if [[ "$ASCIINEMA_MAJOR" -ge 3 ]]; then
  REC=(asciinema rec --headless --window-size "${DEMO_COLS}x${DEMO_ROWS}" -f asciicast-v2)
else
  REC=(asciinema rec --cols "$DEMO_COLS" --rows "$DEMO_ROWS")
fi
TERM=xterm-256color "${REC[@]}" --overwrite -q -i 2 -t "a2a-serve: $DEMO_NAME" \
  -c "tmux -L $DEMO_SOCKET attach -t $DEMO_SESSION" "$CAST" </dev/null >/dev/null 2>&1 &
ASCIINEMA_PID=$!
until_ok 30 sh -c "tmux -L $DEMO_SOCKET list-clients -t $DEMO_SESSION | grep -q ."
log "recording $CAST (watch: tmux -L $DEMO_SOCKET attach -r)"
pause 1

scene

browser_wait
pause 2
# Cut the cast before tmux clears the screen on exit.
cutoff="$(cast_time)"
t kill-server
wait "$ASCIINEMA_PID"
ASCIINEMA_PID=""
awk -v cut="$cutoff" 'NR == 1 { print; next } { split($0, f, /[[,]/) } f[2] + 0 <= cut + 0' "$CAST" >"$CAST.tmp"
mv "$CAST.tmp" "$CAST"

for var in OPENROUTER_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY GEMINI_API_KEY ${DEMO_SECRET:-} ${DEMO_SECRET_VARS:-}; do
  value="${!var:-}"
  if [[ ${#value} -ge 8 ]] && grep -qF -- "$value" "$CAST"; then
    rm -f "$CAST"
    die "the recording showed the value of \$$var; deleted $CAST"
  fi
done

if [[ -n "$GIF" ]]; then
  "$DEMO_TOOLS/compose.sh" "$CAST" "${CAST%.cast}.gif" --speed "$GIF_SPEED"
fi
ls -1 "$DEMO_OUT/$DEMO_NAME".* "$DEMO_OUT/$DEMO_NAME"-browser-*.webm 2>/dev/null || true
