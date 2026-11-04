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
# Renders the Graphviz sources in this directory to PNGs next to them:
# <name>.png for light backgrounds and <name>-dark.png for dark ones, both
# transparent. The dark variant swaps the palette below, so sources should
# only use these colors. Docs show the pair with
#   ![…](graphs/<name>.png#gh-light-mode-only)
#   ![…](graphs/<name>-dark.png#gh-dark-mode-only)
# which GitHub and the site both honor.
#
# Usage:
#   docs/graphs/render.sh               # every .dot file here
#   docs/graphs/render.sh <name>.dot    # only these

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

if ! command -v dot >/dev/null; then
  echo "error: dot not found; install Graphviz (apt install graphviz, brew install graphviz)" >&2
  exit 1
fi

# light=dark: node fill, lines, label text, then cluster fills, cluster
# borders and ellipse fills of each color family.
palette=(
  "#ffffff=#1b1f24" "#555555=#9aa3ad" "#333333=#c9d1d9"
  "#eef5ff=#141c2b" "#9bb7e0=#3d5a85" "#dbe8ff=#1e2b42"
  "#f0f9f0=#13201a" "#9ccc9c=#3f6b4a" "#e3f4e3=#1c2e22"
  "#f3eefb=#1d1828" "#b9a4dd=#5f4d85"
  "#fff4e5=#2a2012" "#e0b070=#8a6a35" "#e0b48a=#8a6a45" "#fdf1e6=#2a2016"
  "#f2f2f2=#22262b" "#f4f4f4=#22262b"
)
swap=()
for pair in "${palette[@]}"; do
  swap+=(-e "s/${pair%=*}/${pair#*=}/gI")
done

if [[ $# -eq 0 ]]; then
  set -- *.dot
fi

for src in "$@"; do
  src="$(basename "$src")"
  name="${src%.dot}"
  dot -Tpng -Gdpi=110 -Gbgcolor=transparent "$src" -o "$name.png"
  sed "${swap[@]}" "$src" |
    dot -Tpng -Gdpi=110 -Gbgcolor=transparent -Gfontcolor="#e6edf3" -Nfontcolor="#e6edf3" -o "$name-dark.png"
  echo "$name.png $name-dark.png"
done
