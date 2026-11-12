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
# summary: examples/07-slim-network: two agents on a SLIM node in Docker, and a SLIM client (needs --build, Docker, cgo)

prepare() {
  command -v docker >/dev/null || die "the example runs the SLIM node in Docker"
  if (exec 3<>/dev/tcp/127.0.0.1/46357) 2>/dev/null; then
    die "port 46357 is in use; the example runs the SLIM node there"
  fi
  log "building the SLIM network provider"
  (cd "$REPO_ROOT/providers/slim" \
    && go run github.com/agntcy/slim-bindings-go/v3/cmd/slim-bindings-setup >/dev/null \
    && go build -o "$DEMO_WORK/bin/netprovider-slim" . \
    && go build -o "$DEMO_WORK/bin/slim-a2a-send" ./cmd/slim-a2a-send) || die "building the SLIM network provider failed"
  mkdir -p "$HOME/a2a-example7"
  ln -s "$REPO_ROOT" "$HOME/a2a-example7/a2a-serve"
  ln -s "$DEMO_WORK/bin/slim-a2a-send" "$HOME/a2a-example7/slim-a2a-send"
}

layout() {
  panes B
  title B "terminal B"
}

cleanup_node() {
  (cd "$REPO_ROOT/examples/07-slim-network" && docker compose down >/dev/null 2>&1) || true
}

scene() {
  trap cleanup_node EXIT
  say B "1. Run the SLIM node"
  run B "cd ~/a2a-example7/a2a-serve/examples/07-slim-network"
  run B "docker compose up -d --wait"
  until_ok 60 bash -c "exec 3<>/dev/tcp/127.0.0.1/46357"
  pause 2

  clear_pane B
  say B "3. Join two agents"
  run B "export SLIM='http://127.0.0.1:46357?namespace=example7&peers=coder,reviewer'"
  run B 'export SLIM_SECRET="$(openssl rand -hex 32)"'
  local name
  for name in coder reviewer; do
    run B "mkdir -p ~/a2a-example7/$name && cd ~/a2a-example7/$name"
    if [[ "$name" == coder ]]; then
      run B "cat > README.md <<'EOF'
# greet

\`greet\` prints a greeting in the language you pick: \`greet --lang fr\` prints \`Bonjour\`.
EOF"
    fi
    run B "printf '%s' \"\$SLIM_SECRET\" | a2a-serve config set --agent $AGENT --name $name $AGENT_FLAGS \\
  --private \"\$SLIM\" --network-provider slim --network-token -"
    run B "a2a-serve start --detach"
  done
  clear_pane B
  run B "cd ~/a2a-example7/coder && a2a-serve status"
  wait_text B 'private  http://slim.local/a2a/example7/coder' 30
  pause 3

  # A peer that wasn't up when coder first looked stays hidden for a minute.
  until_ok 120 sh -c "a2a-serve peers --dir '$HOME/a2a-example7/coder' | grep -q reviewer"
  clear_pane B
  say B "4. Let coder call reviewer"
  run B "a2a-serve peers --dir ~/a2a-example7/coder"
  run B "a2a-serve contact add reviewer --dir ~/a2a-example7/coder"
  run B "a2a send -a \"\$(a2a-serve status --url=local --dir ~/a2a-example7/coder)\" \\
  \"$(fake 'Ask reviewer what it thinks of the README in your workspace.' 'call reviewer hello')\""
  approve_pending B '-a "$(a2a-serve status --url=local --dir ~/a2a-example7/coder)"'
  expect_status B completed
  expect_log "$HOME/a2a-example7/reviewer" 'task completed'
  pause 3

  clear_pane B
  say B "5. Call an agent from a SLIM client"
  run B "~/a2a-example7/slim-a2a-send a2a/example7/reviewer \"$(fake 'What does a good README start with?' hello)\""
  wait_text B '✓ task COMPLETED' 120
  expect_log "$HOME/a2a-example7/reviewer" 'from=node:slim:a2a/example7/slim-a2a-send'
  pause 3

  run B "a2a-serve stop --dir ~/a2a-example7/coder && a2a-serve stop --dir ~/a2a-example7/reviewer"
  run B "docker compose down"
}
