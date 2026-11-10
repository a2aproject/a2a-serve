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
# Stops every agent and leftover process of a simulation run, and lists
# containers that still mount its directory. With --delete, then removes
# the run directory. Usage: tools/sim/cleanup.sh [--root DIR] [--delete]

set -euo pipefail

ROOT="${SIM_ROOT:-/tmp/a2a-sim}"
DELETE=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --root) ROOT="${2:?--root needs a value}"; shift 2 ;;
    --delete) DELETE=1; shift ;;
    *) echo "usage: tools/sim/cleanup.sh [--root DIR] [--delete]" >&2; exit 2 ;;
  esac
done
[[ -f "$ROOT/env.sh" ]] || { echo "cleanup: no run in $ROOT" >&2; exit 1; }
ROOT="$(cd "$ROOT" && pwd)"

# shellcheck source=/dev/null
source "$ROOT/env.sh" cleanup cleanup

status=0
# Agents live in lane directories and in the simulated users' homes. The
# prunes skip the large caches in those homes.
while IFS= read -r cfg; do
  dir="$(dirname "$(dirname "$cfg")")"
  if ! out="$(a2a-serve stop --dir "$dir" 2>&1)"; then
    echo "$dir: $out" >&2
    status=1
  fi
done < <(find "$ROOT/lanes" \( -name node_modules -o -name .cache -o -name .npm -o -path '*/go/pkg' -o -path '*/.a2a/state' \) -prune \
  -o -path '*/.a2a/config.yaml' -print 2>/dev/null)

# Lanes start relays, webhook servers and other helpers in the background.
# Stop the processes that run from the run directory, except this script
# and the shells that started it.
keep=" "
pid=$$
while [[ "$pid" -gt 1 ]]; do
  keep+="$pid "
  # The command name in /proc/PID/stat can hold spaces; the parent PID is the
  # second field after it.
  pid="$(sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null | cut -d' ' -f2)"
  [[ -n "$pid" ]] || break
done
stray=()
for p in /proc/[0-9]*; do
  pid="${p#/proc/}"
  [[ "$keep" == *" $pid "* ]] && continue
  cwd="$(readlink "$p/cwd" 2>/dev/null)" || continue
  exe="$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)" || continue
  if [[ "$cwd" == "$ROOT"/* || "$cwd" == "$ROOT" || "$exe" == "$ROOT"/* ]]; then
    stray+=("$pid")
  fi
done
if [[ ${#stray[@]} -gt 0 ]]; then
  echo "stopping ${#stray[@]} processes of $ROOT: ${stray[*]}" >&2
  kill "${stray[@]}" 2>/dev/null || true
  sleep 2
  kill -9 "${stray[@]}" 2>/dev/null || true
fi

left="$(docker ps -q | xargs -r docker inspect --format '{{.Name}} {{range .Mounts}}{{.Source}} {{end}}' | grep -F "$ROOT" | cut -d' ' -f1 || true)"
if [[ -n "$left" ]]; then
  echo "containers still running from $ROOT:" >&2
  echo "$left" >&2
  status=1
fi

if [[ "$DELETE" == 1 ]]; then
  if [[ "$status" != 0 ]]; then
    echo "cleanup: kept $ROOT, since the run didn't stop cleanly" >&2
    exit "$status"
  fi
  # Go's module cache is read-only.
  chmod -R u+w "$ROOT"
  rm -rf "$ROOT"
  echo "deleted $ROOT"
fi
exit "$status"
