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
# Injects random faults into the agents of a simulation run while its lanes
# test them, and logs each fault to $ROOT/chaos.log. It only touches servers
# started from the run's build and containers of agents under the run
# directory. See tools/sim/SKILL.md.

set -euo pipefail

ALL_FAULTS="container-kill,container-pause,net-cut,server-freeze,server-restart,server-kill"

usage() {
  cat <<EOF
usage: tools/sim/chaos.sh [flags]

  --root DIR          run directory (default \$SIM_ROOT or /tmp/a2a-sim)
  --seed N            random seed, logged so a round can be replayed (default: random)
  --interval MIN:MAX  seconds between faults (default 60:180)
  --hold MIN:MAX      seconds a pause, cut or freeze lasts, and before a killed server restarts (default 5:30)
  --minutes N         stop after N minutes (default 25)
  --faults LIST       comma-separated subset of: $ALL_FAULTS (default: all)
  --once FAULT        inject one fault now and exit
EOF
}

ROOT="${SIM_ROOT:-/tmp/a2a-sim}"
SEED="$(( $(date +%s) % 100000 ))"
INTERVAL=60:180
HOLD=5:30
MINUTES=25
FAULTS="$ALL_FAULTS"
ONCE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --root) ROOT="${2:?}"; shift 2 ;;
    --seed) SEED="${2:?}"; shift 2 ;;
    --interval) INTERVAL="${2:?}"; shift 2 ;;
    --hold) HOLD="${2:?}"; shift 2 ;;
    --minutes) MINUTES="${2:?}"; shift 2 ;;
    --faults) FAULTS="${2:?}"; shift 2 ;;
    --once) ONCE="${2:?}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ -f "$ROOT/env.sh" ]] || { echo "chaos: no run in $ROOT; run tools/sim/setup.sh first" >&2; exit 1; }

# shellcheck source=/dev/null
source "$ROOT/env.sh" chaos chaos
LOG="$ROOT/chaos.log"
RANDOM="$SEED"

log() { printf '%s\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" | tee -a "$LOG"; }
between() { local lo="${1%:*}" hi="${1#*:}"; echo $(( lo + RANDOM % (hi - lo + 1) )); }
pick() { local n=$#; [[ $n -gt 0 ]] || return 1; shift $(( RANDOM % n )); echo "$1"; }

containers() {
  docker ps --filter label=a2a.dir --format '{{.Names}} {{.Label "a2a.dir"}}' |
    awk -v root="$ROOT/" 'index($2, root) == 1 { print $1 }'
}

servers() { pgrep -f "^$ROOT/bin/a2a-serve start" || true; }

server_dir() {
  local pid="$1" prev="" arg
  while IFS= read -r -d '' arg; do
    [[ "$prev" == --dir ]] && { echo "$arg"; return; }
    [[ "$arg" == --dir=* ]] && { echo "${arg#--dir=}"; return; }
    prev="$arg"
  done < "/proc/$pid/cmdline"
  readlink "/proc/$pid/cwd"
}

# as_server runs a command with the environment of a server process, so a
# restart writes the owner token to the HOME the server was started from.
as_server() {
  local pid="$1"; shift
  local -a envs
  mapfile -d '' envs < "/proc/$pid/environ"
  env -i "${envs[@]}" "$@"
}

inject() {
  local fault="$1" target hold
  hold="$(between "$HOLD")"
  case "$fault" in
    container-kill)
      target="$(pick $(containers))" || { log "skip $fault: no containers"; return; }
      log "$fault $target"
      docker kill "$target" >/dev/null ;;
    container-pause)
      target="$(pick $(containers))" || { log "skip $fault: no containers"; return; }
      log "$fault $target for ${hold}s"
      docker pause "$target" >/dev/null
      sleep "$hold"
      docker unpause "$target" >/dev/null 2>&1 || log "unpause $target failed" ;;
    net-cut)
      target="$(pick $(containers))" || { log "skip $fault: no containers"; return; }
      if docker network disconnect bridge "$target" >/dev/null 2>&1; then
        log "$fault $target for ${hold}s"
        sleep "$hold"
        docker network connect bridge "$target" >/dev/null 2>&1 || log "reconnect $target failed"
        return
      fi
      # A container behind the egress boundary has no network. Its PID 1,
      # a2a-init, carries every connection of the agent to the boundary, and
      # the agent's stdio and server socket bypass it. Freezing it stalls
      # open connections and new ones, as a cut network does.
      local init
      init="$(docker inspect --format '{{.State.Pid}}' "$target" 2>/dev/null)"
      [[ -n "$init" && "$(tr '\0' ' ' < "/proc/$init/cmdline" 2>/dev/null)" == *a2a-init* ]] ||
        { log "skip $fault: $target has no bridge network and no a2a-init"; return; }
      log "$fault $target for ${hold}s (froze a2a-init, pid $init)"
      kill -STOP "$init"
      sleep "$hold"
      kill -CONT "$init" 2>/dev/null || log "resume a2a-init of $target failed" ;;
    server-freeze)
      target="$(pick $(servers))" || { log "skip $fault: no servers"; return; }
      log "$fault pid $target ($(server_dir "$target")) for ${hold}s"
      kill -STOP "$target"
      sleep "$hold"
      kill -CONT "$target" 2>/dev/null || log "resume $target failed" ;;
    server-restart)
      target="$(pick $(servers))" || { log "skip $fault: no servers"; return; }
      local dir; dir="$(server_dir "$target")"
      log "$fault $dir"
      as_server "$target" "$ROOT/bin/a2a-serve" restart --dir "$dir" >/dev/null 2>&1 || log "restart $dir failed" ;;
    server-kill)
      target="$(pick $(servers))" || { log "skip $fault: no servers"; return; }
      local dir; dir="$(server_dir "$target")"
      local -a envs
      mapfile -d '' envs < "/proc/$target/environ"
      log "$fault pid $target ($dir), restart in ${hold}s"
      kill -9 "$target"
      sleep "$hold"
      env -i "${envs[@]}" "$ROOT/bin/a2a-serve" start --dir "$dir" --detach --quiet >/dev/null 2>&1 || log "start $dir after kill failed" ;;
    *) echo "chaos: unknown fault $fault" >&2; exit 2 ;;
  esac
}

if [[ -n "$ONCE" ]]; then
  inject "$ONCE"
  exit 0
fi

IFS=, read -r -a faults <<< "$FAULTS"
log "start seed=$SEED interval=$INTERVAL hold=$HOLD minutes=$MINUTES faults=$FAULTS"
end=$(( $(date +%s) + MINUTES * 60 ))
while :; do
  sleep "$(between "$INTERVAL")"
  [[ $(date +%s) -lt $end ]] || break
  inject "$(pick "${faults[@]}")"
done
log "stop"
