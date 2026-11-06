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
# Creates a small git project for a simulated agent to work in.
# Usage: tools/sim/newproj.sh <dir>

set -euo pipefail

dir="${1:?usage: newproj.sh <dir>}"
mkdir -p "$dir"
cd "$dir"
git init -q
printf '# calc\n\nA tiny calculator.\n' > README.md
printf 'def add(a, b):\n    return a + b\n\n\ndef div(a, b):\n    return a / b\n' > calc.py
git add -A
git -c user.email=sim@example.com -c user.name=sim commit -qm init
