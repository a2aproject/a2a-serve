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
# Renders the diagram sources in this directory to PNGs next to them:
# Graphviz (.dot) with dot, Mermaid (.mmd) with mermaid-cli through npx.
# Mermaid uses the system Chrome when it finds one (or $CHROME).
#
# Usage:
#   docs/graphs/render.sh                      # every .dot and .mmd file here
#   docs/graphs/render.sh access-model-*.dot   # only these

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

if [[ $# -eq 0 ]]; then
  shopt -s nullglob
  set -- *.dot *.mmd
fi

need() {
  if ! command -v "$1" >/dev/null; then
    echo "error: $1 not found; $2" >&2
    exit 1
  fi
}

puppeteer_cfg=""
mermaid() {
  need npx "install Node.js to render Mermaid diagrams"
  local chrome="${CHROME:-}"
  if [[ -z "$chrome" ]]; then
    for c in google-chrome google-chrome-stable chromium chromium-browser \
      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
      if command -v "$c" >/dev/null || [[ -x "$c" ]]; then
        chrome="$(command -v "$c" || echo "$c")"
        break
      fi
    done
  fi
  local args=()
  if [[ -n "$chrome" ]]; then
    if [[ -z "$puppeteer_cfg" ]]; then
      puppeteer_cfg="$(mktemp --suffix=.json 2>/dev/null || mktemp)"
      trap 'rm -f "$puppeteer_cfg"' EXIT
      printf '{"executablePath": "%s", "args": ["--no-sandbox"]}\n' "$chrome" >"$puppeteer_cfg"
    fi
    args=(-p "$puppeteer_cfg")
    export PUPPETEER_SKIP_DOWNLOAD=1
  fi
  npx -y @mermaid-js/mermaid-cli@11 -q "${args[@]}" -i "$1" -o "$2" -b white -w 1600 -s 1 >/dev/null
}

for src in "$@"; do
  src="$(basename "$src")"
  case "$src" in
    *.dot)
      need dot "install Graphviz (apt install graphviz, brew install graphviz)"
      png="${src%.dot}.png"
      dot -Tpng -Gdpi=110 "$src" -o "$png"
      ;;
    *.mmd)
      png="${src%.mmd}.png"
      mermaid "$src" "$png"
      ;;
    *)
      echo "error: $src: want a .dot or .mmd file" >&2
      exit 1
      ;;
  esac
  size="$(python3 -c 'import struct,sys; d=open(sys.argv[1],"rb").read(24); print("%dx%d" % struct.unpack(">II", d[16:24]))' "$png" 2>/dev/null || echo "?")"
  echo "$png ($size)"
done
