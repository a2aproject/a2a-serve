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
# Prints one line per finding of every lane of a run, as
# "severity<TAB>lane#n<TAB>category<TAB>title", sorted by severity.
# Usage: tools/sim/collect.sh [--root DIR]

set -euo pipefail

ROOT="${SIM_ROOT:-/tmp/a2a-sim}"
[[ "${1:-}" == --root ]] && ROOT="${2:?--root needs a value}"

for f in "$ROOT"/lanes/*/findings.md; do
  [[ -f "$f" ]] || continue
  lane="$(basename "$(dirname "$f")")"
  awk -v lane="$lane" '
    function flush() { if (title != "") print sev "\t" lane "#" n "\t" cat "\t" title }
    /^## [0-9]+\./ { flush(); n = $2; sub(/\.$/, "", n); title = $0; sub(/^## [0-9]+\. */, "", title); sev = "?"; cat = "?"; next }
    /^## / { flush(); title = ""; next }
    tolower($0) ~ /^(- )?\**severity/ { s = tolower($0); sev = (s ~ /high/) ? "high" : (s ~ /medium/) ? "medium" : "low" }
    tolower($0) ~ /^(- )?\**category/ { c = $0; sub(/^[^:]*: */, "", c); gsub(/\*/, "", c); sub(/^ +/, "", c); cat = c }
    END { flush() }
  ' "$f"
done | sort -t$'\t' -k1,1 -s | awk -F'\t' '{ o = ($1 == "high") ? 0 : ($1 == "medium") ? 1 : 2; print o "\t" $0 }' | sort -n -s -k1,1 | cut -f2-
