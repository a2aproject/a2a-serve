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

# The node publishes its port on NODE_PORT, and slimtap listens on the
# example's port in front of it. The commands run as the README writes them,
# and the top pane shows every message the node routes.
NODE_PORT=46359

prepare() {
  command -v docker >/dev/null || die "the example runs the SLIM node in Docker"
  local port
  for port in 46357 "$NODE_PORT"; do
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      die "port $port is in use; the demo runs the SLIM node there"
    fi
  done
  log "building the SLIM network provider"
  (cd "$REPO_ROOT/providers/slim" \
    && go run github.com/agntcy/slim-bindings-go/v3/cmd/slim-bindings-setup >/dev/null \
    && go build -o "$DEMO_WORK/bin/netprovider-slim" . \
    && go build -o "$DEMO_WORK/bin/slim-a2a-send" ./cmd/slim-a2a-send) || die "building the SLIM network provider failed"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$DEMO_WORK/bin/slimtap" ./tools/demo/slimtap) || die "building slimtap failed"
  mkdir -p "$HOME/a2a-example7"
  ln -s "$REPO_ROOT" "$HOME/a2a-example7/a2a-serve"
  ln -s "$DEMO_WORK/bin/slim-a2a-send" "$HOME/a2a-example7/slim-a2a-send"
  cat >"$DEMO_WORK/slim-port.yaml" <<YAML
services:
  slim:
    ports: !override
      - "127.0.0.1:$NODE_PORT:46357"
YAML
  export COMPOSE_FILE="$REPO_ROOT/examples/07-slim-network/compose.yaml:$DEMO_WORK/slim-port.yaml"
  # Terminal 1 saves SLIM and SLIM_SECRET at each prompt, and terminal 2
  # loads them when it restarts, as if they were copied over.
  cat >>"$DEMO_WORK/bashrc" <<RC
[[ \$DEMO_PANE == coder ]] && PROMPT_COMMAND="declare -p SLIM SLIM_SECRET >$DEMO_WORK/slim-env 2>/dev/null; \$PROMPT_COMMAND"
[[ \$DEMO_PANE == reviewer && -f $DEMO_WORK/slim-env ]] && source $DEMO_WORK/slim-env
RC
}

layout() {
  panes coder
  split_pane node coder above 40
  split_pane reviewer coder right 50
  title node "what the SLIM node sees: every message it routes"
  title coder "terminal 1: coder"
  title reviewer "terminal 2: reviewer"
  t respawn-pane -k -t "$(pane_id node)" "slimtap 127.0.0.1:46357 127.0.0.1:$NODE_PORT"
}

teardown() {
  (cd "$REPO_ROOT/examples/07-slim-network" && docker compose down >/dev/null 2>&1) || true
}

# both <command 1> <command 2> runs a command in each terminal at once.
both() {
  run coder "$1" &
  local a=$!
  run reviewer "$2" &
  local b=$!
  wait "$a" || die "terminal 1 failed"
  wait "$b" || die "terminal 2 failed"
}

join() {
  printf '%s' "printf '%s' \"\$SLIM_SECRET\" | a2a-serve config set --agent $AGENT --name $1 $AGENT_FLAGS \\
  --private \"\$SLIM\" --network-provider slim --network-token -"
}

scene() {
  say coder "No VPN, no open ports: agents dial out to one SLIM node."
  run coder "cd ~/a2a-example7/a2a-serve/examples/07-slim-network"
  run coder "docker compose up -d --wait"
  until_ok 60 bash -c "exec 3<>/dev/tcp/127.0.0.1/$NODE_PORT"
  run coder "export SLIM='http://127.0.0.1:46357?namespace=example7&peers=coder,reviewer'"
  run coder 'export SLIM_SECRET="$(openssl rand -hex 32)"'
  t respawn-pane -k -t "$(pane_id reviewer)" "$(_pane_shell reviewer)"
  wait_ready reviewer 30

  clear_pane coder
  both "mkdir -p ~/a2a-example7/coder && cd ~/a2a-example7/coder" \
    "# the same SLIM and SLIM_SECRET as terminal 1"
  both "cat > README.md <<'EOF'
# greet

\`greet\` prints a greeting in the language you pick: \`greet --lang fr\` prints \`Bonjour\`.
EOF" "mkdir -p ~/a2a-example7/reviewer && cd ~/a2a-example7/reviewer"
  both "$(join coder)" "$(join reviewer)"
  clear_pane coder
  clear_pane reviewer
  both "a2a-serve start --detach" "a2a-serve start --detach"
  wait_text coder 'private  http://slim.local/a2a/example7/coder' 30
  pause 2

  # A peer that wasn't up when coder first looked stays hidden for a minute.
  until_ok 120 sh -c "a2a-serve peers --dir '$HOME/a2a-example7/coder' | grep -q reviewer"
  clear_pane coder
  clear_pane reviewer
  run reviewer "# who calls reviewer, from its log"
  start reviewer "a2a-serve logs -f -n 0 | grep --line-buffered -o 'from=.*'"
  run coder "a2a-serve contact add reviewer --dir ~/a2a-example7/coder"
  clear_pane coder
  run coder "a2a send -a \"\$(a2a-serve status --url=local --dir ~/a2a-example7/coder)\" \\
  \"$(fake 'Ask reviewer what it thinks of the README in your workspace.' 'call reviewer hello')\""
  approve_pending coder '-a "$(a2a-serve status --url=local --dir ~/a2a-example7/coder)"'
  expect_status coder completed
  expect_log "$HOME/a2a-example7/reviewer" 'task completed'
  pause 2

  clear_pane coder
  run coder "~/a2a-example7/slim-a2a-send a2a/example7/reviewer \"$(fake 'What does a good README start with?' hello)\""
  wait_text coder '✓ task COMPLETED' 120
  expect_log "$HOME/a2a-example7/reviewer" 'from=node:slim:a2a/example7/slim-a2a-send'
  say coder "The node routed every message by name and read none of them: MLS encrypts end to end."
  pause 4
}
