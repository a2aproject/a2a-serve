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
# summary: examples/05-relay-cloud-run/README.md: a relay on Cloud Run, joined by two invited agents (needs --build, gcloud, DEMO_PROJECT)

PROMPT='Ask reviewer for one tip on writing a good commit message, and tell me what it said.'

prepare() {
  [[ -n "${DEMO_PROJECT:-}" ]] || die "set DEMO_PROJECT to the Google Cloud project to create the relay in"
  command -v gcloud >/dev/null || die "gcloud is not installed"
  # gcloud keeps its sign-in under the real HOME.
  export CLOUDSDK_CONFIG="${CLOUDSDK_CONFIG:-$DEMO_REAL_HOME/.config/gcloud}"
  export CLOUDSDK_CORE_PROJECT="$DEMO_PROJECT" CLOUDSDK_CORE_DISABLE_PROMPTS=1
  cp "$DEMO_TOOLS/scenarios/05-relay-cloud-run/setup.sh" "$HOME/setup.sh"
  # A rerun starts from a new service and an empty database, so the relay
  # has no members of earlier runs.
  gcloud run services delete a2a-serve-relay --region us-central1 --quiet >/dev/null 2>&1 || true
  gcloud sql databases delete relay --instance "${DEMO_SQL_INSTANCE:-a2a-serve-relay-db}" --quiet >/dev/null 2>&1 || true
  # The slow part of setup.sh, creating the Cloud SQL instance, runs here.
  # It also exports what the agents' panes need: URL and RELAY_ADMIN_TOKEN.
  log "running setup.sh (off camera; a new Cloud SQL instance takes about 10 minutes)"
  # shellcheck source=05-relay-cloud-run/setup.sh
  source "$HOME/setup.sh" >/dev/null || die "setup.sh failed"
  if [[ -n "${DEMO_RELAY_IMAGE:-}" ]]; then
    # Deploys DEMO_RELAY_IMAGE where the guide names the published image, for
    # recordings made before a release publishes it.
    export DEMO_RELAY_IMAGE
    cat >>"$DEMO_WORK/bashrc" <<'EOF'
gcloud() {
  local a args=()
  for a in "$@"; do
    args+=("${a//ghcr.io\/a2aproject\/a2a-serve-relay:/$DEMO_RELAY_IMAGE:}")
  done
  command gcloud "${args[@]}"
}
EOF
  fi
}

layout() {
  panes R
  split_pane C R below 40
  split_pane V C right 50
  title R "relay"
  title C "coder"
  title V "reviewer"
}

# join <pane> <name> [flags] invites an agent and starts it on the relay.
join() {
  run "$1" "a2a-serve relay invite $2 --url \"\$URL\" --admin-token env:RELAY_ADMIN_TOKEN"
  local token
  token="$(last_match "$1" 'mesh-bt-[A-Za-z0-9._-]+')"
  run "$1" "mkdir -p ~/$2 && cd ~/$2"
  start "$1" "a2a-serve config set --agent $AGENT --name $2 $AGENT_FLAGS ${3:-}\\
  --private \"\$URL\" --public \"\$URL\" --network-token -"
  wait_text "$1" 'network-token: '
  pause 1
  # Pasted at the prompt, which doesn't echo it.
  t send-keys -t "$(pane_id "$1")" -l -- "$token"
  t send-keys -t "$(pane_id "$1")" Enter
  wait_ready "$1"
  clear_pane "$1"
  start "$1" "a2a-serve start --quiet"
}

# wait_public waits until the relay answers 10 times in a row: a new
# service's public access takes a while to apply everywhere.
wait_public() {
  local ok=0 tries=0
  while ((ok < 10)); do
    ((tries++ < 200)) || die "the relay isn't public at $URL"
    if curl -fsS -o /dev/null "$URL/.well-known/a2a-serve-relay" 2>/dev/null; then
      ok=$((ok + 1))
    else
      ok=0
    fi
    sleep 3
  done
}

scene() {
  say R "Steps 1-3 of the guide in one script: database, secrets, service account"
  run R "source setup.sh" 600

  say R "4. Deploy the relay"
  paste_run R "gcloud run deploy a2a-serve-relay --region \"\$REGION\" \\
  --image \"ghcr.io/a2aproject/a2a-serve-relay:\$VERSION\" \\
  --service-account \"\$SA\" --allow-unauthenticated \\
  --min-instances 1 --max-instances 1 --no-cpu-throttling \\
  --concurrency 1000 --timeout 3600 \\
  --add-cloudsql-instances \"\$DB_INSTANCE\" \\
  --set-secrets RELAY_ADMIN_TOKEN=relay-admin-token:latest,RELAY_DATABASE=relay-database:latest \\
  --args=\"--listen=:8080,--url=\$URL,--admin-token=env:RELAY_ADMIN_TOKEN,--database=env:RELAY_DATABASE\"" 900
  wait_public

  clear_pane R
  say R "5. Check the relay"
  run R "a2a-serve relay status --url \"\$URL\" --admin-token env:RELAY_ADMIN_TOKEN"

  join C coder "--allow-tools execute "
  join V reviewer
  wait_text C 'public +https' 300
  wait_text V 'public +https' 300
  pause 2

  # A new agent can take half a minute to show up on the relay.
  until_ok 120 sh -c "a2a-serve peers --dir '$HOME/coder' | grep -q reviewer"
  clear_pane R
  say R "6. Let coder call reviewer through the relay"
  run R "a2a-serve contact add reviewer --dir ~/coder"
  run R "a2a send -a \"\$(a2a-serve status --url=local --dir ~/coder)\" \\
  \"$(fake "$PROMPT" 'call reviewer hello')\"" 600
  pause 8
}
