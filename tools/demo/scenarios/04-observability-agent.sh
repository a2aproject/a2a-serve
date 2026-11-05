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
# summary: examples/04-observability-agent: an agent from a recipe investigating a local Grafana with generated telemetry

DEMO_SECRET_VARS="GRAFANA_SERVICE_ACCOUNT_TOKEN"

O11Y_NET="a2a-demo-o11y"
O11Y_LGTM="a2a-demo-lgtm"
O11Y_GEN="ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:latest"
O11Y_RECIPE="https://github.com/a2aproject/a2a-serve/tree/main/examples/04-observability-agent/recipe"

# gen starts a telemetrygen container that sends to the LGTM stack until
# teardown removes it.
gen() {
  local name="$1"
  shift
  docker run -d --rm --name "a2a-demo-gen-$name" --network "$O11Y_NET" "$O11Y_GEN" "$@" \
    --otlp-endpoint "$O11Y_LGTM:4317" --otlp-insecure --duration inf >/dev/null
}

loki_has_logs() {
  curl -fsS -G -H "Authorization: Bearer $GRAFANA_SERVICE_ACCOUNT_TOKEN" \
    --data-urlencode 'query=sum(count_over_time({service_name="checkout"}[5m]))' \
    http://localhost:3000/api/datasources/proxy/uid/loki/loki/api/v1/query | grep -q '"value"'
}

# prepare runs the "Try it on your machine" stack of the example off camera,
# with telemetry for three services: checkout is slow and fails often,
# payments fails sometimes, and api is healthy.
prepare() {
  [[ -z "$DEMO_FAKE" ]] || die "this scenario runs real agents against Grafana; it has no --fake mode"
  command -v jq >/dev/null || die "jq is not installed"
  if curl -fsS -o /dev/null --max-time 2 http://localhost:3000 2>/dev/null; then
    die "port 3000 is in use; the example runs Grafana there"
  fi
  # The demo answers n to github, as a reader without a token would.
  unset GITHUB_TOKEN

  docker network create "$O11Y_NET" >/dev/null
  docker run -d --name "$O11Y_LGTM" --network "$O11Y_NET" -p 3000:3000 -p 4317:4317 -p 4318:4318 grafana/otel-lgtm >/dev/null
  until_ok 180 curl -fsS -u admin:admin http://localhost:3000/api/health
  local sa
  sa=$(curl -fsS -u admin:admin -H 'Content-Type: application/json' \
    -d '{"name":"a2a-o11y","role":"Viewer"}' http://localhost:3000/api/serviceaccounts | jq .id)
  GRAFANA_SERVICE_ACCOUNT_TOKEN=$(curl -fsS -u admin:admin -H 'Content-Type: application/json' \
    -d '{"name":"a2a-o11y"}' "http://localhost:3000/api/serviceaccounts/$sa/tokens" | jq -r .key)
  export GRAFANA_SERVICE_ACCOUNT_TOKEN GRAFANA_URL=http://host.docker.internal:3000

  gen checkout-ok traces --service checkout --rate 4 --span-duration 120ms --status-code Ok --child-spans 2
  gen checkout-err traces --service checkout --rate 1.5 --span-duration 900ms --status-code Error --child-spans 2
  gen payments-ok traces --service payments --rate 2 --span-duration 200ms --status-code Ok
  gen payments-err traces --service payments --rate 0.5 --span-duration 400ms --status-code Error
  gen api-ok traces --service api --rate 6 --span-duration 40ms --status-code Ok
  gen checkout-log logs --service checkout --rate 1.5 --batch-size 10 --severity-text ERROR --severity-number 17 \
    --body "POST /checkout failed: payment gateway timeout after 900ms"
  gen payments-log logs --service payments --rate 0.5 --batch-size 10 --severity-text ERROR --severity-number 17 \
    --body "card authorization failed: upstream returned 503"
  gen api-log logs --service api --rate 2 --batch-size 10 --body "GET /api/products 200"
  log "collecting telemetry for two minutes"
  until_ok 180 loki_has_logs
  sleep 120
}

teardown() {
  docker ps -aq --filter "name=a2a-demo-gen-" | xargs docker rm -f >/dev/null 2>&1
  docker rm -f "$O11Y_LGTM" >/dev/null 2>&1
  docker network rm "$O11Y_NET" >/dev/null 2>&1 || log "could not remove the docker network $O11Y_NET"
}

layout() {
  panes B
  title B "terminal"
}

scene() {
  say B "2. Create the agent from the recipe"
  run B "cd ~"
  start B "a2a-serve recipe $O11Y_RECIPE --name o11y"
  answer B 'Create o11y in \./o11y\? \(y, n\) \[y\]: *$' y
  answer B 'API keys to pass to the agent .*\]: *$' ""
  answer B 'Use github .*\? \(y, n\) \[y\]: *$' n
  answer B 'GRAFANA_SERVICE_ACCOUNT_TOKEN is set in this shell.*\[y\]: *$' y
  answer B 'GRAFANA_URL is set in this shell.*\[y\]: *$' y
  wait_ready B
  set_model "$HOME/o11y"
  pause 3

  clear_pane B
  say B "3. Describe your environment"
  run B "cd ~/o11y"
  run B "a2a-serve start --detach"
  run B 'a2a send --history 0 -a "$(a2a-serve status --url)" "Learn the environment."'
  # The agent may look outside its workspace, such as at its own config.
  reply_pending B reject '--history 0 -a "$(a2a-serve status --url)"'
  pause 4

  clear_pane B
  say B "6. Ask it"
  run B 'a2a send --history 0 -a "$(a2a-serve status --url)" "Which services return the most errors this hour?"'
  reply_pending B reject '--history 0 -a "$(a2a-serve status --url)"'
  pause 5
}
