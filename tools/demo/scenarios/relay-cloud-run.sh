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
# summary: docs/guide/relay-cloud-run.md: a relay on Cloud Run with Cloud SQL, joined by two invited agents (needs --build, gcloud, DEMO_PROJECT)

prepare() {
  [[ -n "${DEMO_PROJECT:-}" ]] || die "set DEMO_PROJECT to the Google Cloud project to create the relay in"
  command -v gcloud >/dev/null || die "gcloud is not installed"
  # gcloud keeps its sign-in under the real HOME.
  export CLOUDSDK_CONFIG="${CLOUDSDK_CONFIG:-$DEMO_REAL_HOME/.config/gcloud}"
  export CLOUDSDK_CORE_DISABLE_PROMPTS=1 CLOUDSDK_CORE_DISABLE_PROGRESS_TRACKER=1
  gcloud projects describe "$DEMO_PROJECT" >/dev/null || die "gcloud can't read the project $DEMO_PROJECT"
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
  panes T
  title T "terminal"
}

# join <name> joins an agent to the relay with an invite.
join() {
  run T "a2a-serve relay invite $1 --url \"\$URL\" --admin-token env:RELAY_ADMIN_TOKEN"
  local token
  token="$(last_match T 'mesh-bt-[A-Za-z0-9._-]+')"
  pause 2
  run T "mkdir -p ~/$1 && cd ~/$1"
  start T "a2a-serve config set --agent $AGENT --name $1 $AGENT_FLAGS \\
  --private \"\$URL\" --public \"\$URL\" --relay-token -"
  wait_text T 'relay-token: '
  pause 1
  # Pasted at the prompt, which doesn't echo it.
  t send-keys -t "$(pane_id T)" -l -- "$token"
  t send-keys -t "$(pane_id T)" Enter
  wait_ready T
  run T "a2a-serve start --detach"
}

scene() {
  setup_step
  database_step
  secrets_step
  account_step
  deploy_step
  check_step
  agents_step
}

setup_step() {
  say T "Run a relay on Cloud Run: docs/guide/relay-cloud-run.md"
  run T "export CLOUDSDK_CORE_PROJECT=$DEMO_PROJECT"
  run T "REGION=us-central1"
  run T "VERSION=v0.1.0"
  run T "gcloud services enable run.googleapis.com sqladmin.googleapis.com secretmanager.googleapis.com" 300
  run T "URL=\"https://a2a-serve-relay-\$(gcloud projects describe \"\$CLOUDSDK_CORE_PROJECT\" --format='value(projectNumber)').\$REGION.run.app\""
}

database_step() {
  clear_pane T
  say T "1. Create the database"
  run T "gcloud sql instances create a2a-serve-relay-db --region \"\$REGION\" \\
  --database-version POSTGRES_17 --edition enterprise --tier db-f1-micro" 1800
  run T "gcloud sql databases create relay --instance a2a-serve-relay-db" 300
  run T "DB_PASSWORD=\$(openssl rand -hex 24)"
  run T "gcloud sql users create relay --instance a2a-serve-relay-db --password \"\$DB_PASSWORD\"" 300
  run T "DB_INSTANCE=\$(gcloud sql instances describe a2a-serve-relay-db --format='value(connectionName)')"
}

secrets_step() {
  clear_pane T
  say T "2. Store the secrets"
  run T "printf %s \"\$(openssl rand -hex 32)\" | gcloud secrets create relay-admin-token --data-file=-"
  run T "printf 'host=/cloudsql/%s user=relay password=%s dbname=relay sslmode=disable' \"\$DB_INSTANCE\" \"\$DB_PASSWORD\" |
  gcloud secrets create relay-database --data-file=-"
}

account_step() {
  clear_pane T
  say T "3. Create a service account"
  run T "gcloud iam service-accounts create a2a-serve-relay"
  run T "SA=\"a2a-serve-relay@\$CLOUDSDK_CORE_PROJECT.iam.gserviceaccount.com\""
  run T "gcloud projects add-iam-policy-binding \"\$CLOUDSDK_CORE_PROJECT\" \\
  --member \"serviceAccount:\$SA\" --role roles/cloudsql.client --condition None --format none"
  run T "for s in relay-admin-token relay-database; do
  gcloud secrets add-iam-policy-binding \"\$s\" --member \"serviceAccount:\$SA\" \\
    --role roles/secretmanager.secretAccessor --format none
done"
}

deploy_step() {
  # IAM grants can take a minute to apply; the recording drops the idle time.
  sleep 60
  clear_pane T
  say T "4. Deploy the relay, for invited agents"
  run T "gcloud run deploy a2a-serve-relay --region \"\$REGION\" \\
  --image \"ghcr.io/a2aproject/a2a-serve-relay:\$VERSION\" \\
  --service-account \"\$SA\" --allow-unauthenticated \\
  --min-instances 1 --max-instances 1 --no-cpu-throttling \\
  --concurrency 1000 --timeout 3600 \\
  --add-cloudsql-instances \"\$DB_INSTANCE\" \\
  --set-secrets RELAY_ADMIN_TOKEN=relay-admin-token:latest,RELAY_DATABASE=relay-database:latest \\
  --args=\"--listen=:8080,--url=\$URL,--admin-token=env:RELAY_ADMIN_TOKEN,--database=env:RELAY_DATABASE\"" 900
}

check_step() {
  clear_pane T
  say T "5. Check the relay"
  run T "curl -s \"\$URL/.well-known/a2a-serve-relay\""
  run T "export RELAY_ADMIN_TOKEN=\$(gcloud secrets versions access latest --secret relay-admin-token)"
  run T "a2a-serve relay status --url \"\$URL\" --admin-token env:RELAY_ADMIN_TOKEN"
  pause 3
}

agents_step() {
  clear_pane T
  say T "6. Invite two agents"
  join coder
  clear_pane T
  join reviewer
  clear_pane T
  run T "a2a-serve relay members --url \"\$URL\" --admin-token env:RELAY_ADMIN_TOKEN"
  pause 2

  # A new agent can take half a minute to show up on the relay.
  until_ok 120 sh -c "a2a-serve peers --dir '$HOME/coder' | grep -q reviewer"
  clear_pane T
  say T "7. Let coder call reviewer through the relay"
  run T "a2a-serve peers --dir ~/coder"
  run T "a2a-serve contact add reviewer --dir ~/coder"
  run T "a2a send -a \"\$(a2a-serve status --url=local --dir ~/coder)\" \\
  \"$(fake 'Ask reviewer what it thinks of the README of this project.' 'call reviewer hello')\""
  pause 3

  run T "a2a-serve stop --dir ~/coder && a2a-serve stop --dir ~/reviewer"
}
