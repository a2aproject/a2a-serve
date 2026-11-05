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
# Renders a cast as a GIF and splices in the browser videos that record.sh
# noted in <name>.markers, each where the terminal handed over to it.
#
#   tools/demo/compose.sh <name>.cast <out.gif> [--speed N] [--fps N]

set -euo pipefail

usage() {
  echo "usage: tools/demo/compose.sh <name>.cast <out.gif> [--speed N] [--fps N]" >&2
  exit 2
}

[[ $# -ge 2 ]] || usage
CAST="$1"
OUT="$2"
shift 2
SPEED=1.5
FPS=12
while [[ $# -gt 0 ]]; do
  case "$1" in
    --speed) SPEED="$2"; shift 2 ;;
    --fps) FPS="$2"; shift 2 ;;
    *) usage ;;
  esac
done

for tool in agg ffmpeg ffprobe; do
  command -v "$tool" >/dev/null || { echo "error: $tool is not installed" >&2; exit 1; }
done

MARKERS="${CAST%.cast}.markers"
if [[ ! -s "$MARKERS" ]]; then
  agg --speed "$SPEED" --idle-time-limit 2 "$CAST" "$OUT"
  exit 0
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/a2a-demo-compose.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
agg --speed "$SPEED" --idle-time-limit 2 "$CAST" "$WORK/term.gif"

size="$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$WORK/term.gif")"
W="${size%x*}"
H="${size#*x}"
# The browser videos are padded with the terminal's background color.
BG="$(ffmpeg -v error -i "$WORK/term.gif" -frames:v 1 -vf crop=1:1:0:0 -f rawvideo -pix_fmt rgb24 - | od -An -tx1 | tr -d ' \n')"

inputs=(-i "$WORK/term.gif")
filter="[0:v]fps=$FPS,setsar=1,format=rgb24,split=SPLITS"
segments=""
prev=0
n=0
dir="$(dirname "$CAST")"
while read -r at video; do
  [[ -n "$video" ]] || continue
  [[ -f "$dir/$video" ]] || { echo "error: $dir/$video is missing" >&2; exit 1; }
  # The cast plays faster in the GIF, so its times shrink by the same factor.
  at="$(awk -v t="$at" -v s="$SPEED" 'BEGIN { printf "%.3f", t / s }')"
  n=$((n + 1))
  inputs+=(-i "$dir/$video")
  filter="$filter[t$n]"
  segments="$segments[t$n]trim=start=$prev:end=$at,setpts=PTS-STARTPTS[s$n];"
  segments="$segments[$n:v]setpts=PTS/$SPEED,fps=$FPS,scale=$W:$H:force_original_aspect_ratio=decrease,pad=$W:$H:(ow-iw)/2:(oh-ih)/2:color=0x$BG,setsar=1,format=rgb24[b$n];"
  prev="$at"
done <"$MARKERS"

filter="${filter/SPLITS/$((n + 1))}[tlast];$segments[tlast]trim=start=$prev,setpts=PTS-STARTPTS[slast];"
concat=""
for ((i = 1; i <= n; i++)); do
  concat="$concat[s$i][b$i]"
done
filter="$filter${concat}[slast]concat=n=$((2 * n + 1)):v=1:a=0,split[g][p];[p]palettegen=stats_mode=diff[pal];[g][pal]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle"

ffmpeg -v error -y "${inputs[@]}" -filter_complex "$filter" "$OUT"
