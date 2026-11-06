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
# summary: examples/02-tailscale-telegram-grok: tailnet, Telegram (browser) and an optional Grok node; real accounts only

DEMO_SECRET_VARS="CODER_BOT_TOKEN GROK_WEBHOOK_TOKEN GROK_WEBHOOK_URL"

# The agent's tailnet identity outlives the run, so later runs reuse the
# machine "coder" instead of adding coder-1, coder-2 and asking for approval.
TSNET_CACHE="$DEMO_REAL_HOME/.cache/a2a-demo/tsnet-coder"
TSNET_DIR="$HOME/a2a-example2/coder/.a2a/state/tsnet"

prepare() {
  [[ -z "$DEMO_FAKE" ]] || die "this scenario needs Tailscale and Telegram; it has no --fake mode"
  [[ -n "${CODER_BOT_TOKEN:-}" ]] || die "export CODER_BOT_TOKEN, the token of the bot from @BotFather"
  [[ -d "$DEMO_TOOLS/browser/node_modules" ]] || die "run npm install in tools/demo/browser"
  if [[ -n "${GROK_WEBHOOK_URL:-}" && -z "${GROK_WEBHOOK_TOKEN:-}" ]]; then
    die "GROK_WEBHOOK_URL is set without GROK_WEBHOOK_TOKEN"
  fi
}

teardown() {
  [[ -d "$TSNET_DIR" ]] || return 0
  rm -rf "$TSNET_CACHE" && mkdir -p "$(dirname "$TSNET_CACHE")" && cp -R "$TSNET_DIR" "$TSNET_CACHE" ||
    log "could not save the tailnet identity to $TSNET_CACHE"
}

layout() {
  panes B
  split_pane A B above 35
  title A "terminal A: coder"
  title B "terminal B"
}

scene() {
  say B "3. Start the agent"
  run B "mkdir -p ~/a2a-example2/coder && cd ~/a2a-example2/coder"
  run B "a2a-serve config set --agent $AGENT --name coder $AGENT_FLAGS \\
  --private tailscale --public tailscale"
  run B "a2a-serve messenger add telegram --token env:CODER_BOT_TOKEN"
  if [[ -d "$TSNET_CACHE" ]]; then
    mkdir -p "$(dirname "$TSNET_DIR")" && cp -R "$TSNET_CACHE" "$TSNET_DIR"
  fi

  run A "cd ~/a2a-example2/coder"
  start A "a2a-serve start"
  wait_text A 'login\.tailscale\.com|telegram +@'
  if screen A | grep -q 'login\.tailscale\.com'; then
    ask_human "approve the machine at $(last_match A 'https://login\.tailscale\.com/[^ ]+') and disable its key expiry" \
      A 't\.me/[^ ]+\?start='
  fi
  wait_text A 't\.me/[^ ]+\?start=[A-Za-z0-9-]+'
  local link
  link="$(last_match A 'https://t\.me/[^ ]+\?start=[A-Za-z0-9-]+')"
  pause 2

  clear_pane B
  say B "4. Link Telegram, then chat and approve a tool call"
  browser telegram "link=$link" approve=1 \
    "message=Create a file notes.md with three ideas for a weekend project."
  browser_wait

  local private
  private="$(a2a-serve status --url=private --dir "$HOME/a2a-example2/coder")"
  clear_pane B
  say B "5. Call it from your other machines"
  run B "a2a send -a $private \"What's in notes.md?\""
  approve_pending B "-a $private"
  pause 3

  [[ -n "${GROK_WEBHOOK_URL:-}" ]] || return 0

  clear_pane B
  say B "6. Add the Grok bot as a webhook node"
  run B 'a2a-serve pair invite grok \
  --webhook "$GROK_WEBHOOK_URL" \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN" \
  --description "Grok: web search and current events"'
  ask_human "paste the message from terminal B into the Grok bot's instructions and let it pair"
  until_ok 1800 sh -c "a2a-serve pair list --dir '$HOME/a2a-example2/coder' | grep grok | grep -vq 'not paired'"
  clear_pane B
  run B "a2a-serve pair list"
  pause 2

  say B "8. Delegate to Grok from Telegram"
  local bot
  bot="$(last_match A 'telegram +@[A-Za-z0-9_]+')"
  browser telegram "bot=${bot##*@}" approve=1 settle=300 \
    "message=Ask grok what the top AI news is today, and summarize it in three bullets."
  browser_wait
}
