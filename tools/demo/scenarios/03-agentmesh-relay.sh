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
  if ! command -v agentmesh-one >/dev/null; then
    log "installing agentmesh-one"
    # The module proxy occasionally resets streams.
    for attempt in 1 2 3; do
      GOBIN="$DEMO_WORK/bin" CGO_ENABLED=0 go install github.com/google/agentmesh/cmd/agentmesh-one@v0.2.0 && break
      [[ $attempt -lt 3 ]] || die "installing agentmesh-one failed"
      sleep $((attempt * 15))
    done
  fi
  if curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:8443/readyz 2>/dev/null; then
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
  run R "mkdir -p ~/a2a-example3/relay"
  run R "cat > ~/a2a-example3/relay/policy.json <<'JSON'
{
  \"roles\": [
    {\"name\": \"mesh-admin\", \"allowed_services\": [\"*\"], \"allowed_targets\": [\"*\"], \"allowed_ingress\": [{\"services\": [\"*\"]}]},
    {\"name\": \"mesh:role:router\", \"allowed_services\": [\"*\"], \"allowed_targets\": [\"*\"]},
    {
      \"name\": \"mesh:role:node\",
      \"allowed_services\": [\"*\"], \"allowed_targets\": [\"*\"], \"allowed_publish\": [\"*\"], \"allowed_labels\": [\"*\"],
      \"allowed_ingress\": [{\"services\": [\"a2a://*\"], \"allowed_auth_modes\": [\"INGRESS_AUTH_MODE_PASSTHROUGH\"]}]
    }
  ]
}
JSON"
  clear_pane R
  start R "agentmesh-one --bind-address 127.0.0.1 --port 8443 \\
  --data-dir ~/a2a-example3/relay --policy-file ~/a2a-example3/relay/policy.json"
  until_ok 60 test -s "$HOME/a2a-example3/relay/join-token"
  until_ok 60 curl -fsS http://127.0.0.1:8443/readyz
  pause 2

  run B 'export RELAY=http://127.0.0.1:8443'
  run B 'export RELAY_TOKEN="$(cat ~/a2a-example3/relay/join-token)"'

  clear_pane B
  say B "2. Join two agents"
  local name
  for name in coder reviewer; do
    run B "mkdir -p ~/a2a-example3/$name && cd ~/a2a-example3/$name"
    if [[ "$name" == coder ]]; then
      run B "cat > README.md <<'EOF'
# greet

\`greet\` prints a greeting in the language you pick: \`greet --lang fr\` prints \`Bonjour\`.
EOF"
    fi
    run B "a2a-serve config set --agent $AGENT --name $name $AGENT_FLAGS \\
  --private \"\$RELAY\" --public \"\$RELAY\" --network-token env:RELAY_TOKEN"
    run B "a2a-serve start --detach"
  done
  clear_pane B
  run B "cd ~/a2a-example3/coder && a2a-serve status"
  pause 3

  # A new agent can take half a minute to show up on the relay.
  until_ok 120 sh -c "a2a-serve peers --dir '$HOME/a2a-example3/coder' | grep -q reviewer"
  clear_pane B
  say B "3. Let coder call reviewer"
  run B "a2a-serve peers --dir ~/a2a-example3/coder"
  run B "a2a-serve contact add reviewer --dir ~/a2a-example3/coder"
  run B "a2a send -a \"\$(a2a-serve status --url=local --dir ~/a2a-example3/coder)\" \\
  \"$(fake 'Ask reviewer what it thinks of the README in your workspace.' 'call reviewer hello')\""
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
  run B "a2a send -a \"\$PUBLIC\" \"$(fake 'What does this project do?' hello)\""
  approve_pending B '-a "$PUBLIC"'
  expect_status B completed
  pause 3

  run B "a2a-serve stop --dir ~/a2a-example3/coder && a2a-serve stop --dir ~/a2a-example3/reviewer"
}
