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
# Helpers that scenarios use to drive tmux panes while asciinema records them.
# record.sh sources this file; it must stay compatible with bash 3.2 (macOS).

DEMO_SOCKET="${DEMO_SOCKET:-a2a-demo}"
DEMO_SESSION="demo"
DEMO_TYPE_DELAY="${DEMO_TYPE_DELAY:-0.035}"
DEMO_PAUSE_SCALE="${DEMO_PAUSE_SCALE:-1}"
DEMO_TIMEOUT="${DEMO_TIMEOUT:-600}"

t() {
  tmux -L "$DEMO_SOCKET" "$@"
}

log() {
  printf '\033[2m[demo] %s\033[0m\n' "$*" >&2
}

die() {
  printf '[demo] error: %s\n' "$*" >&2
  exit 1
}

# pane_id prints the tmux pane ID of the pane named $1.
pane_id() {
  local id
  id="$(t list-panes -s -t "$DEMO_SESSION" -F '#{@name} #{pane_id}' | awk -v n="$1" '$1 == n { print $2 }')"
  [[ -n "$id" ]] || die "no pane named $1"
  printf '%s' "$id"
}

_pane_shell() {
  printf 'DEMO_PANE=%q exec bash --noprofile --rcfile %q -i' "$1" "$DEMO_WORK/bashrc"
}

_name_pane() {
  t set-option -p -t "$1" @name "$2"
  wait_ready "$2" 30
}

# panes creates the session with its first pane, named $1. Further names
# are split off with split_pane.
panes() {
  t new-session -d -s "$DEMO_SESSION" -x "$DEMO_COLS" -y "$DEMO_ROWS" "$(_pane_shell "$1")"
  t set-option -g status off
  t set-option -g pane-border-status top
  t set-option -g pane-border-format ' #{?@title,#{@title},#{@name}} '
  t set-option -g pane-active-border-style fg=cyan
  _name_pane "$(t display-message -p -t "$DEMO_SESSION" '#{pane_id}')" "$1"
}

# split_pane <new> <from> <above|below|left|right> <percent> adds pane <new>
# next to <from>, taking <percent> of its space.
split_pane() {
  local flags
  case "$3" in
    above) flags="-vb" ;;
    below) flags="-v" ;;
    left) flags="-hb" ;;
    right) flags="-h" ;;
    *) die "split_pane: bad direction $3" ;;
  esac
  local id
  id="$(t split-window "$flags" -l "$4%" -t "$(pane_id "$2")" -P -F '#{pane_id}' "$(_pane_shell "$1")")"
  _name_pane "$id" "$1"
}

# title <pane> <text> sets the label on the pane's border.
title() {
  t set-option -p -t "$(pane_id "$1")" @title "$2"
}

focus() {
  t select-pane -t "$(pane_id "$1")"
}

# wait_ready <pane> [timeout] waits for the next prompt of the pane's shell.
wait_ready() {
  local timeout="${2:-$DEMO_TIMEOUT}"
  t wait-for "demo-ready-$1" &
  local pid=$! waited=0
  while kill -0 "$pid" 2>/dev/null; do
    if ((waited >= timeout * 10)); then
      kill "$pid" 2>/dev/null
      die "pane $1: no prompt after ${timeout}s"
    fi
    sleep 0.1
    waited=$((waited + 1))
  done
  wait "$pid"
}

# type_keys <pane> <text> types text into the pane without pressing Enter.
type_keys() {
  local id text="$2" i c
  id="$(pane_id "$1")"
  for ((i = 0; i < ${#text}; i++)); do
    c="${text:i:1}"
    if [[ "$c" == $'\n' ]]; then
      t send-keys -t "$id" Enter
    else
      t send-keys -t "$id" -l -- "$c"
    fi
    sleep "$DEMO_TYPE_DELAY"
  done
}

# run <pane> <command> types the command, runs it and waits for the prompt.
run() {
  focus "$1"
  type_keys "$1" "$2"
  sleep 0.3
  t send-keys -t "$(pane_id "$1")" Enter
  wait_ready "$1" "${3:-$DEMO_TIMEOUT}"
  pause 1
}

# start <pane> <command> types and runs a command that keeps running.
start() {
  focus "$1"
  type_keys "$1" "$2"
  sleep 0.3
  t send-keys -t "$(pane_id "$1")" Enter
}

# say <pane> <text> types a comment line, as narration.
say() {
  run "$1" "# $2"
}

clear_pane() {
  t send-keys -t "$(pane_id "$1")" C-l
  sleep 0.3
}

interrupt() {
  t send-keys -t "$(pane_id "$1")" C-c
  wait_ready "$1" 60
}

pause() {
  sleep "$(awk -v s="$1" -v k="$DEMO_PAUSE_SCALE" 'BEGIN { print s * k }')"
}

screen() {
  t capture-pane -p -J -S -500 -t "$(pane_id "$1")"
}

# wait_text <pane> <ERE> [timeout] waits until the pane shows a match.
wait_text() {
  local timeout="${3:-$DEMO_TIMEOUT}" start
  start="$(date +%s)"
  until screen "$1" | grep -Eq -- "$2"; do
    if (($(date +%s) - start >= timeout)); then
      die "pane $1: no match for /$2/ after ${timeout}s"
    fi
    sleep 0.5
  done
}

# last_match <pane> <ERE> prints the last match of the ERE on the pane.
last_match() {
  local m
  m="$(screen "$1" | grep -Eo -- "$2" | tail -n 1)"
  [[ -n "$m" ]] || die "pane $1: no match for /$2/"
  printf '%s' "$m"
}

# until_ok <timeout> <command...> retries a command off camera until it
# succeeds, for waits that would only bore the viewer.
until_ok() {
  local timeout="$1" start
  shift
  start="$(date +%s)"
  until "$@" >/dev/null 2>&1; do
    if (($(date +%s) - start >= timeout)); then
      die "timed out after ${timeout}s: $*"
    fi
    sleep 1
  done
}

# fake <real> <fake> prints <fake> in fake mode and <real> otherwise, for
# prompts that fakeacp has to understand.
fake() {
  if [[ -n "$DEMO_FAKE" ]]; then
    printf '%s' "$2"
  else
    printf '%s' "$1"
  fi
}

# browser <scenario> [key=value...] starts a browser recording of
# browser/<scenario>.mjs in the background, to <name>-browser-<n>.webm.
# browser_wait waits for it.
DEMO_BROWSER_COUNT=0
browser() {
  local name="$1"
  shift
  DEMO_BROWSER_COUNT=$((DEMO_BROWSER_COUNT + 1))
  node "$DEMO_TOOLS/browser/record.mjs" "$DEMO_TOOLS/browser/$name.mjs" \
    --out "$DEMO_OUT/$DEMO_NAME-browser-$DEMO_BROWSER_COUNT.webm" "$@" >&2 &
  DEMO_BROWSER_PID=$!
}

browser_wait() {
  [[ -n "${DEMO_BROWSER_PID:-}" ]] || return 0
  wait "$DEMO_BROWSER_PID" || die "browser recording failed"
  DEMO_BROWSER_PID=
}

# ask_human <message> [<pane> <ERE> [timeout]] tells the person running the
# recording to do something, and waits for <pane> to show <ERE> as the sign
# that it is done.
ask_human() {
  printf '\n\033[1;33m[demo] ACTION NEEDED: %s\033[0m\n' "$1" >&2
  [[ $# -ge 3 ]] || return 0
  wait_text "$2" "$3" "${4:-1800}"
}
