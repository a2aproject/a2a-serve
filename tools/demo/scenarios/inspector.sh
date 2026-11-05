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
# summary: docs/guide/inspector.md: answer an approval in the inspector (terminal + browser)

layout() {
  panes B
  split_pane A B above 35
  title A "terminal A: coder"
  title B "terminal B"
}

scene() {
  run B "mkdir -p ~/coder && cd ~/coder"
  run A "cd ~/coder"
  start A "a2a-serve start $AGENT --name coder $AGENT_FLAGS"
  wait_text A 'local +http'
  pause 1

  run B 'export A2ACLI_AGENT_CARD=$(a2a-serve status --url=local)'
  run B "a2a send \"$(fake "Use bash to write the output of 'uname -sr' into system.txt." ask)\""
  pause 2
  local task
  task="$(last_match B 'Task: +[^ ]+')"

  clear_pane B
  say B "Answer the approval in the inspector"
  run B "a2a-serve inspect --no-browser"
  browser inspector "url=$(last_match B 'http://[^ ]+/inspect/login[^ ]*')"
  browser_wait

  clear_pane B
  run B "a2a task get ${task##* }"
  pause 3
}
