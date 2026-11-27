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
# summary: examples/03-agentmesh-relay: two agents on a local Agent Mesh relay (needs --build)

prepare() {
  command -v jq >/dev/null || die "jq is needed for the relay invites"
  if curl -sS -o /dev/null --max-time 2 http://127.0.0.1:8443/ 2>/dev/null; then
    die "port 8443 is in use; the example runs the relay there"
  fi
}

layout() {
  panes B
  split_pane R B above 30
  title R "relay"
  title B "terminal B"
}

scene() {
  say R "1. Run the relay"
  run R 'mkdir -p ~/a2a-example3/relay && cd ~/a2a-example3/relay'
  start R 'a2a-serve relay run --url http://127.0.0.1:8443 --listen 127.0.0.1:8443'
  until_ok 120 bash -o pipefail -c 'a2a-serve relay status --dir "$1" -o json | jq -e ".running"' \
    _ "$HOME/a2a-example3/relay"
  pause 2

  run B 'export RELAY=http://127.0.0.1:8443'

  clear_pane B
  say B "2. Join two agents"
  local name
  for name in coder reviewer; do
    run B "RELAY_TOKEN=\"\$(a2a-serve relay invite $name --dir ~/a2a-example3/relay -o json | jq -er .token)\""
    run B "mkdir -p ~/a2a-example3/$name && cd ~/a2a-example3/$name"
    if [[ "$name" == coder ]]; then
      run B "cat > README.md <<'EOF'
# greet

\`greet\` prints a greeting in the language you pick: \`greet --lang fr\` prints \`Bonjour\`.
EOF"
    fi
    run B "printf '%s' \"\$RELAY_TOKEN\" | a2a-serve config set --agent $AGENT --name $name $AGENT_FLAGS \\
  --private \"\$RELAY\" --public \"\$RELAY\" --network-token -"
    run B "a2a-serve start --detach"
  done
  clear_pane B
  run B "cd ~/a2a-example3/coder && a2a-serve status"
  run B "a2a-serve relay members --dir ~/a2a-example3/relay"
  pause 3

  # A new agent can take half a minute to show up on the relay.
  until_ok 120 sh -c "a2a-serve peers --dir '$HOME/a2a-example3/coder' | grep -q reviewer"
  clear_pane B
  say B "3. Let coder call reviewer"
  run B "a2a-serve peers --dir ~/a2a-example3/coder"
  run B "a2a-serve contact add reviewer --dir ~/a2a-example3/coder"
  run B "a2a send -a \"\$(a2a-serve status --url=local --dir ~/a2a-example3/coder)\" \\
  \"$(fake 'Send reviewer the README of your workspace and ask what it thinks of it.' 'call reviewer hello')\""
  approve_pending B '-a "$(a2a-serve status --url=local --dir ~/a2a-example3/coder)"'
  expect_status B completed
  expect_log "$HOME/a2a-example3/reviewer" 'task completed'
  pause 3

  clear_pane B
  say B "4. Call an agent from outside the relay"
  run B 'PUBLIC="$(a2a-serve status --url=public --dir ~/a2a-example3/coder)"'
  run B "a2a-serve pair invite me --dir ~/a2a-example3/coder"
  local code
  code="$(last_match B 'code [A-Z0-9]{4}-[A-Z0-9]{4}')"
  pause 2
  run B "a2a-serve pair join \"\$PUBLIC\" ${code#code }"
  run B "a2a send -a \"\$PUBLIC\" \"$(fake 'What does this project do?' 'ask six times')\""
  approve_pending B '-a "$PUBLIC"' 10
  expect_status B completed
  pause 3

  run B "a2a-serve stop --dir ~/a2a-example3/coder && a2a-serve stop --dir ~/a2a-example3/reviewer"
  interrupt R
}
