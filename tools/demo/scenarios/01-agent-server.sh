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
# summary: examples/01-agent-server: serve an agent, approve a tool call, call a second agent

layout() {
  panes B
  split_pane A B above 35
  title A "terminal A: coder"
  title B "terminal B"
}

scene() {
  say B "1. Create a workspace"
  run B "mkdir -p ~/a2a-example1/coder && cd ~/a2a-example1/coder"
  run B "cat > AGENTS.md <<'EOF'
You are reached over A2A. Keep answers to one short sentence.
EOF"

  say A "2. Start the agent"
  run A "cd ~/a2a-example1/coder"
  start A "a2a-serve start $AGENT --name coder $AGENT_FLAGS"
  wait_text A 'local +http'
  pause 2

  clear_pane B
  run B "a2a-serve status"
  pause 2

  say B "3. Save the agent's URL so the commands stay short"
  run B 'cat > .env <<EOF
A2ACLI_AGENT_CARD=$(a2a-serve status --url=local)
A2ACLI_TIMEOUT=3m
EOF'

  clear_pane B
  say B "4. Ask a question"
  run B "a2a send \"$(fake 'In one sentence, what can you do?' hello)\""
  pause 3

  clear_pane B
  say B "5. Approve a tool call"
  run B "a2a send \"$(fake "Use bash to write the output of 'uname -sr' into system.txt." ask)\""
  pause 3
  local task
  task="$(last_match B 'a2a send --task-id [^ ]+')"
  run B "${task% *} ${task##* } approve"
  [[ -n "$DEMO_FAKE" ]] || run B "cat system.txt"
  pause 2

  local context
  context="$(last_match B 'Context: +[^ ]+')"
  clear_pane B
  say B "6. Continue the conversation"
  run B "a2a send --context-id ${context##* } \"$(fake 'Which file did you just create?' hello)\""
  pause 3

  clear_pane B
  say B "7. Let it call another agent"
  run B "mkdir -p ~/a2a-example1/reviewer && cd ~/a2a-example1/reviewer"
  run B "a2a-serve start $AGENT --name reviewer $AGENT_FLAGS --detach"
  run B "a2a-serve status"
  run B "a2a-serve pair invite coder"
  local code url
  code="$(last_match B 'code [A-Z0-9]{4}-[A-Z0-9]{4}')"
  url="$(a2a-serve status --url=local --dir "$HOME/a2a-example1/reviewer")"
  pause 2
  clear_pane B
  run B "cd ~/a2a-example1/coder"
  run B "a2a-serve contact add reviewer --url $url --code ${code#code }"
  run B "a2a send \"$(fake 'Ask the reviewer agent what files it sees in its workspace, and tell me.' 'call reviewer hello')\""
  approve_pending B
  pause 4
}
