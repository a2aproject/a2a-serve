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
# Steps 1-3 of docs/guide/relay-cloud-run.md as one script: the database, the
# secrets and the service account. Source it with CLOUDSDK_CORE_PROJECT set.
# It skips what exists, so the relay-cloud-run demo runs it off camera first
# and again on camera. DEMO_SQL_INSTANCE names another Cloud SQL instance,
# since a deleted instance's name can't be reused for a week.

REGION=us-central1
VERSION=v0.1.0
URL="https://a2a-serve-relay-$(gcloud projects describe "$CLOUDSDK_CORE_PROJECT" --format='value(projectNumber)').$REGION.run.app"
SA="a2a-serve-relay@$CLOUDSDK_CORE_PROJECT.iam.gserviceaccount.com"
export REGION VERSION URL SA

# q runs gcloud quietly, and prints its output only when it fails.
q() {
  local out
  out="$(gcloud --quiet "$@" 2>&1)" || { printf '%s\n' "$out" >&2; return 1; }
}

relay_setup() {
  local db="${DEMO_SQL_INSTANCE:-a2a-serve-relay-db}" password s
  q services enable run.googleapis.com sqladmin.googleapis.com secretmanager.googleapis.com || return
  if ! q sql instances describe "$db" >/dev/null 2>&1; then
    q sql instances create "$db" --region "$REGION" \
      --database-version POSTGRES_17 --edition enterprise --tier db-f1-micro || return
  fi
  if ! q sql databases describe relay --instance "$db" >/dev/null 2>&1; then
    q sql databases create relay --instance "$db" || return
  fi
  DB_INSTANCE="$(gcloud sql instances describe "$db" --format='value(connectionName)')" || return
  export DB_INSTANCE
  echo "✓ database relay on the Cloud SQL instance $DB_INSTANCE"

  if ! q secrets describe relay-database >/dev/null 2>&1; then
    password="$(openssl rand -hex 24)"
    if gcloud sql users list --instance "$db" --format='value(name)' | grep -x relay >/dev/null; then
      q sql users set-password relay --instance "$db" --password "$password" || return
    else
      q sql users create relay --instance "$db" --password "$password" || return
    fi
    printf 'host=/cloudsql/%s user=relay password=%s dbname=relay sslmode=disable' "$DB_INSTANCE" "$password" |
      q secrets create relay-database --data-file=- || return
  fi
  if ! q secrets describe relay-admin-token >/dev/null 2>&1; then
    printf %s "$(openssl rand -hex 32)" | q secrets create relay-admin-token --data-file=- || return
  fi
  RELAY_ADMIN_TOKEN="$(gcloud secrets versions access latest --secret relay-admin-token)" || return
  export RELAY_ADMIN_TOKEN
  echo "✓ secrets relay-admin-token and relay-database in Secret Manager"

  if ! q iam service-accounts describe "$SA" >/dev/null 2>&1; then
    q iam service-accounts create a2a-serve-relay || return
  fi
  q projects add-iam-policy-binding "$CLOUDSDK_CORE_PROJECT" \
    --member "serviceAccount:$SA" --role roles/cloudsql.client --condition None || return
  for s in relay-admin-token relay-database; do
    q secrets add-iam-policy-binding "$s" --member "serviceAccount:$SA" \
      --role roles/secretmanager.secretAccessor || return
  done
  echo "✓ service account $SA"
}

relay_setup
