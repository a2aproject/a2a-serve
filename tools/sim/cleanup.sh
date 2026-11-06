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
# Stops every agent of a simulation run and lists containers that still
# mount its directory. Usage: tools/sim/cleanup.sh [--root DIR]

set -euo pipefail

ROOT="${SIM_ROOT:-/tmp/a2a-sim}"
[[ "${1:-}" == --root ]] && ROOT="${2:?--root needs a value}"
[[ -f "$ROOT/env.sh" ]] || { echo "cleanup: no run in $ROOT" >&2; exit 1; }

# shellcheck source=/dev/null
source "$ROOT/env.sh" cleanup cleanup

status=0
while IFS= read -r cfg; do
  dir="$(dirname "$(dirname "$cfg")")"
  if ! out="$(a2a-serve stop --dir "$dir" 2>&1)"; then
    echo "$dir: $out" >&2
    status=1
  fi
done < <(find "$ROOT/lanes" -path '*/users' -prune -o -path '*/.a2a/config.yaml' -print 2>/dev/null)

left="$(docker ps -q | xargs -r docker inspect --format '{{.Name}} {{range .Mounts}}{{.Source}} {{end}}' | grep -F "$ROOT" | cut -d' ' -f1 || true)"
if [[ -n "$left" ]]; then
  echo "containers still running from $ROOT:" >&2
  echo "$left" >&2
  status=1
fi
exit "$status"
