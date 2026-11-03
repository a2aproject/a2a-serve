#!/usr/bin/env bash
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
# End-to-end validation of `a2a serve run opencode` running Docker-sandboxed coding
# agents backed by a live LLM on an Agent Mesh relay (`agentmesh-one` v0.2.0 +
# embedded Go SDK `github.com/google/agentmesh/sdk/meshnet`, no sidecar):
#
#   Stage 1 (Token Join + P2P Delegation + Public Ingress):
#     1. Starts `agentmesh-one` v0.2.0 (optionally exposed to the internet via
#        `--tunnel cloudflare`).
#     2. Builds `a2a-serve -tags agentmesh`, runs `a2a-serve doctor --fix`, and
#        starts two Docker-sandboxed `a2a serve run opencode` agents (`coder` and
#        `reviewer`) with `--private "$RELAY" --public "$RELAY" --relay-token`.
#     3. Verifies `a2a serve doctor --online`, discovers `reviewer` over the
#        mesh DHT (`a2a serve peers`), pairs with an invite code, and delegates
#        a live LLM task (`coder -> reviewer`) over authenticated P2P streams.
#     4. Pairs an external caller outside the mesh against the public
#        `/ingress/<peer_id>/a2a/coder` URL (`a2a serve tokens add`) and runs a
#        two-hop delegation (`internet -> relay -> coder -> reviewer`).
#
#   Stage 2 (OIDC Device Sign-In + Same-Owner Zero-Code Recognition):
#     5. Starts a local RS256 OIDC Device-Flow Identity Provider and an
#        `agentmesh-one --issuer <idp> --no-join-token` relay.
#     6. Starts two Docker-sandboxed `a2a serve run opencode` agents (`oidc-coder`
#        and `oidc-reviewer`) without `--relay-token`, follows the printed
#        device sign-in link as `Ben@example.com`, verifies `owner.json`, adds
#        `oidc-reviewer` without `--code`, delegates a live LLM task authorized
#        by verified same-owner Biscuit identity (`Peer.Self == true`), and
#        restarts `oidc-coder` without signing in again.
#
# Usage:
#   bash examples/03-agentmesh-relay/demo.sh [--tunnel cloudflare] [--vertex-project <gcp-project>]

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

PINNED_A2A_CLI="v0.3.2-0.20261030153310-f90e4eb628cd"
AGENTMESH_VERSION="v0.2.0"

CACHE_DIR="${TMPDIR:-/tmp}/a2a-am-cache"
mkdir -p "$CACHE_DIR"

AGENTMESH_ONE_BIN="${AGENTMESH_ONE_BIN:-$(command -v agentmesh-one || echo "$CACHE_DIR/agentmesh-one")}"
TUNNEL_PROVIDER="${TUNNEL_PROVIDER:-}"
HARNESS_IMAGE="${HARNESS_IMAGE:-}"
OPENCODE_MODEL="${OPENCODE_MODEL:-}"
OPENAI_BASE_URL="${OPENAI_BASE_URL:-}"
VERTEX_PROJECT="${VERTEX_PROJECT:-}"
VERTEX_LOCATION="${VERTEX_LOCATION:-us-central1}"
GCLOUD_CONFIG="${GCLOUD_CONFIG:-}"
export A2ACLI_TIMEOUT="${A2ACLI_TIMEOUT:-3m}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --agentmesh-one-bin)
      AGENTMESH_ONE_BIN="$2"
      shift 2
      ;;
    --tunnel)
      TUNNEL_PROVIDER="$2"
      shift 2
      ;;
    --image)
      HARNESS_IMAGE="$2"
      shift 2
      ;;
    --model)
      OPENCODE_MODEL="$2"
      shift 2
      ;;
    --openai-base-url)
      OPENAI_BASE_URL="$2"
      shift 2
      ;;
    --vertex-project)
      VERTEX_PROJECT="$2"
      shift 2
      ;;
    --vertex-location)
      VERTEX_LOCATION="$2"
      shift 2
      ;;
    --gcloud-config)
      GCLOUD_CONFIG="$2"
      shift 2
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

if [[ -n "$VERTEX_PROJECT" ]]; then
  OPENAI_BASE_URL="https://${VERTEX_LOCATION}-aiplatform.googleapis.com/v1/projects/${VERTEX_PROJECT}/locations/${VERTEX_LOCATION}/endpoints/openapi"
  OPENCODE_MODEL="${OPENCODE_MODEL:-google/gemini-2.5-flash}"
  GCLOUD_ARGS=()
  if [[ -n "$GCLOUD_CONFIG" ]]; then
    GCLOUD_ARGS+=(--configuration "$GCLOUD_CONFIG")
  fi
  OPENAI_API_KEY="$(gcloud "${GCLOUD_ARGS[@]}" auth application-default print-access-token 2>/dev/null || gcloud "${GCLOUD_ARGS[@]}" auth print-access-token)"
  export OPENAI_API_KEY
fi
OPENCODE_MODEL="${OPENCODE_MODEL:-opencode/big-pickle}"

if ! docker info >/dev/null 2>&1; then
  echo "error: Docker daemon is not reachable (required for Docker-sandboxed opencode agents)" >&2
  exit 1
fi

WORK="$(mktemp -d /tmp/a2a-serve-am.XXXXXX)"
BIN_DIR="$WORK/bin"
OWNER_HOME="$WORK/owner-home"
EXTERNAL_HOME="$WORK/external-home"
mkdir -p "$BIN_DIR" "$OWNER_HOME" "$EXTERNAL_HOME" \
  "$WORK/hub" "$WORK/oidc-hub" "$WORK/idp" \
  "$WORK/coder" "$WORK/reviewer" "$WORK/oidc-coder" "$WORK/oidc-reviewer"

GOMODCACHE="${GOMODCACHE:-$(go env GOMODCACHE)}"
GOCACHE="${GOCACHE:-$(go env GOCACHE)}"
export GOMODCACHE GOCACHE
export PATH="$BIN_DIR:$PATH"
export HOME="$OWNER_HOME"

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "=== DEMO FAILED (rc=$rc); dumping diagnostic logs ===" >&2
    for logf in "$WORK/hub/agentmesh-one.log" "$WORK/oidc-hub/agentmesh-one.log" "$WORK/idp/idp.log"; do
      if [[ -f "$logf" ]]; then
        echo "--- $logf ---" >&2
        tail -n 60 "$logf" >&2 || true
      fi
    done
    for d in "$WORK/coder" "$WORK/reviewer" "$WORK/oidc-coder" "$WORK/oidc-reviewer"; do
      if [[ -f "$d/.a2a/state/server.log" ]]; then
        echo "--- $d/.a2a/state/server.log ---" >&2
        tail -n 60 "$d/.a2a/state/server.log" >&2 || true
      fi
    done
    for cname in $(docker ps -a --filter "label=a2a.agent" --format '{{.Names}}' 2>/dev/null || true); do
      echo "--- docker logs $cname ---" >&2
      docker logs --tail 40 "$cname" >&2 || true
    done
  fi
  for pid_var in SIGNIN_PID HUB_PID OIDC_HUB_PID IDP_PID; do
    local pid="${!pid_var:-}"
    if [[ -n "$pid" ]]; then
      kill "$pid" >/dev/null 2>&1 || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  for d in "$WORK/coder" "$WORK/reviewer" "$WORK/oidc-coder" "$WORK/oidc-reviewer"; do
    if [[ -d "$d/.a2a" && -x "$BIN_DIR/a2a-serve" ]]; then
      "$BIN_DIR/a2a-serve" stop --dir "$d" >/dev/null 2>&1 || true
    fi
  done
  chmod -R u+w "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

banner() {
  printf '\n\033[1;36m==> %s\033[0m\n' "$*"
}

banner "0. Building a2a-serve (-tags agentmesh) and preparing a2a CLI (${PINNED_A2A_CLI}) + agentmesh-one (${AGENTMESH_VERSION})"
CGO_ENABLED=0 go build -tags agentmesh -trimpath -o "$BIN_DIR/a2a-serve" .

if [[ ! -x "$CACHE_DIR/a2a-${PINNED_A2A_CLI}" ]]; then
  TMP_GOBIN="$(mktemp -d "$WORK/gobin.XXXXXX")"
  GOBIN="$TMP_GOBIN" CGO_ENABLED=0 go install -trimpath "github.com/a2aproject/a2a-cli@${PINNED_A2A_CLI}"
  mv "$TMP_GOBIN/a2a-cli" "$CACHE_DIR/a2a-${PINNED_A2A_CLI}"
fi
cp -f "$CACHE_DIR/a2a-${PINNED_A2A_CLI}" "$BIN_DIR/a2a"
chmod +x "$BIN_DIR/a2a"

if [[ ! -x "$AGENTMESH_ONE_BIN" ]]; then
  mkdir -p "$(dirname "$AGENTMESH_ONE_BIN")"
  TMP_GOBIN="$(mktemp -d "$WORK/gobin-am.XXXXXX")"
  GOBIN="$TMP_GOBIN" CGO_ENABLED=0 go install -trimpath "github.com/google/agentmesh/cmd/agentmesh-one@${AGENTMESH_VERSION}"
  mv "$TMP_GOBIN/agentmesh-one" "$AGENTMESH_ONE_BIN"
fi

HOME="$OWNER_HOME" a2a-serve doctor --fix
HOME="$EXTERNAL_HOME" a2a-serve doctor --fix >/dev/null

IMAGE_ARGS=()
if [[ -n "$HARNESS_IMAGE" ]]; then
  IMAGE_ARGS+=(--image "$HARNESS_IMAGE")
fi

SECRET_ARGS=()
for key_var in OPENROUTER_API_KEY ANTHROPIC_API_KEY OPENAI_API_KEY GEMINI_API_KEY; do
  if [[ -n "${!key_var:-}" ]]; then
    SECRET_ARGS+=(--secret "$key_var")
  fi
done

prepare_agent_workspace() {
  local dir="$1"
  mkdir -p "$dir/.a2a/state/home/.config/opencode"
  chmod 700 "$dir/.a2a" "$dir/.a2a/state" "$dir/.a2a/state/home" "$dir/.a2a/state/home/.config" "$dir/.a2a/state/home/.config/opencode"
  cat >"$dir/AGENTS.md" <<EOF
Your workspace directory is ${dir}. You are reached over A2A. Keep answers to one short sentence, always repeat any requested token or identifier verbatim, and always read workspace files under ${dir}/ (for example ${dir}/NOTE.txt, never /NOTE.txt).
EOF
  python3 -c '
import json, os, sys
path, model, base_url = sys.argv[1], sys.argv[2], sys.argv[3]
cfg = {
    "$schema": "https://opencode.ai/config.json",
    "autoupdate": False,
    "model": model,
    "permission": {"bash": "ask", "edit": "ask", "webfetch": "ask"},
}
if base_url:
    model_id = model.removeprefix("custom-openai/")
    cfg["model"] = f"custom-openai/{model_id}"
    cfg["small_model"] = f"custom-openai/{model_id}"
    cfg["provider"] = {
        "custom-openai": {
            "npm": "@ai-sdk/openai-compatible",
            "name": "OpenAI-Compatible Endpoint",
            "options": {
                "baseURL": base_url,
                "apiKey": "{env:OPENAI_API_KEY}",
            },
            "models": {
                model_id: {
                    "name": model_id,
                    "tool_call": True,
                }
            },
        }
    }
with open(path, "w", encoding="utf-8") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)
' "$dir/.a2a/state/home/.config/opencode/opencode.json" "$OPENCODE_MODEL" "$OPENAI_BASE_URL"
}

REVIEW_TOKEN="MESH-REVIEW-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
EXT_TOKEN="PUBLIC-INGRESS-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"
OIDC_TOKEN="OIDC-OWNER-$(python3 -c 'import secrets; print(secrets.token_hex(6))')"

prepare_agent_workspace "$WORK/coder"
prepare_agent_workspace "$WORK/reviewer"
prepare_agent_workspace "$WORK/oidc-coder"
prepare_agent_workspace "$WORK/oidc-reviewer"

printf 'Internal review token: %s\n' "$REVIEW_TOKEN" >"$WORK/reviewer/NOTE.txt"
printf 'External ingress verification token: %s\n' "$EXT_TOKEN" >"$WORK/reviewer/PUBLIC_CHECK.txt"
printf 'Same-owner OIDC review token: %s\n' "$OIDC_TOKEN" >"$WORK/oidc-reviewer/OIDC_NOTE.txt"

cat >"$WORK/hub/policy.json" <<'EOF'
{
  "roles": [
    {
      "name": "mesh-admin",
      "allowed_services": ["*"],
      "allowed_targets": ["*"],
      "allowed_ingress": [{"services": ["*"]}]
    },
    {
      "name": "mesh:role:router",
      "allowed_services": ["*"],
      "allowed_targets": ["*"]
    },
    {
      "name": "mesh:role:node",
      "allowed_services": ["*"],
      "allowed_targets": ["*"],
      "allowed_publish": ["*"],
      "allowed_labels": ["*"],
      "allowed_ingress": [
        {
          "services": ["a2a://*"],
          "allowed_auth_modes": ["INGRESS_AUTH_MODE_PASSTHROUGH"]
        }
      ]
    }
  ],
  "bindings": [
    {
      "role": "mesh:role:node",
      "members": ["mesh:system:authenticated"]
    }
  ]
}
EOF
cp -f "$WORK/hub/policy.json" "$WORK/oidc-hub/policy.json"

if [[ -n "$TUNNEL_PROVIDER" ]]; then
  TUNNEL_CACHE_DIR="${TMPDIR:-/tmp}/a2a-am-tunnel-cache"
  mkdir -p "$TUNNEL_CACHE_DIR" "$WORK/hub/bin"
  if [[ -x "$TUNNEL_CACHE_DIR/cloudflared" ]]; then
    cp -f "$TUNNEL_CACHE_DIR/cloudflared" "$WORK/hub/bin/cloudflared"
  fi
  banner "1. Starting agentmesh-one relay with public --tunnel ${TUNNEL_PROVIDER}"
  "$AGENTMESH_ONE_BIN" \
    --bind-address 127.0.0.1 \
    --port 0 \
    --data-dir "$WORK/hub" \
    --policy-file "$WORK/hub/policy.json" \
    --tunnel "$TUNNEL_PROVIDER" \
    --tunnel-install \
    --router-allow-loopback=false \
    >"$WORK/hub/agentmesh-one.log" 2>&1 &
  HUB_PID=$!

  RELAY=""
  for _ in $(seq 1 300); do
    if ! kill -0 "$HUB_PID" 2>/dev/null; then
      echo "error: agentmesh-one exited while opening tunnel" >&2
      exit 1
    fi
    if [[ -f "$WORK/hub/agentmesh-one.log" ]]; then
      RELAY="$(sed -n 's/^API URL:[[:space:]]*\(https\{0,1\}:\/\/[^[:space:]]*\).*/\1/p' "$WORK/hub/agentmesh-one.log" | head -n1)"
      if [[ -n "$RELAY" && -s "$WORK/hub/join-token" ]]; then
        break
      fi
    fi
    sleep 0.2
  done
  if [[ -z "$RELAY" ]]; then
    echo "error: timed out waiting for public tunnel URL from agentmesh-one" >&2
    exit 1
  fi
  if [[ -x "$WORK/hub/bin/cloudflared" && ! -x "$TUNNEL_CACHE_DIR/cloudflared" ]]; then
    cp -f "$WORK/hub/bin/cloudflared" "$TUNNEL_CACHE_DIR/cloudflared"
  fi
  for _ in $(seq 1 150); do
    if curl -fsS --max-time 4 "${RELAY}/readyz" >/dev/null 2>&1; then
      break
    fi
    sleep 0.4
  done
else
  HUB_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
  RELAY="http://127.0.0.1:${HUB_PORT}"
  banner "1. Starting local agentmesh-one relay at ${RELAY}"
  "$AGENTMESH_ONE_BIN" \
    --bind-address 127.0.0.1 \
    --port "$HUB_PORT" \
    --data-dir "$WORK/hub" \
    --policy-file "$WORK/hub/policy.json" \
    >"$WORK/hub/agentmesh-one.log" 2>&1 &
  HUB_PID=$!
  for _ in $(seq 1 100); do
    if [[ -s "$WORK/hub/join-token" ]] && curl -fsS "${RELAY}/readyz" >/dev/null 2>&1; then
      break
    fi
    sleep 0.2
  done
fi

curl -fsS "${RELAY}/readyz" >/dev/null
RELAY_TOKEN="$(tr -d '\r\n' <"$WORK/hub/join-token")"
export RELAY RELAY_TOKEN
echo "Agent Mesh relay ready at: ${RELAY}"

banner "2. Starting two Docker-sandboxed opencode agents (coder & reviewer) with --private/--public ${RELAY}"
a2a serve config set --dir "$WORK/coder" --harness opencode \
  --name coder \
  --description "Primary coding agent on Agent Mesh" \
  "${IMAGE_ARGS[@]}" \
  --allow-tools read,search,execute \
  --private "$RELAY" \
  --public "$RELAY" \
  --relay-token env:RELAY_TOKEN \
  "${SECRET_ARGS[@]}"
a2a serve run --dir "$WORK/coder" --quiet --detach

a2a serve config set --dir "$WORK/reviewer" --harness opencode \
  --name reviewer \
  --description "Code review agent on Agent Mesh" \
  "${IMAGE_ARGS[@]}" \
  --allow-tools read,search \
  --private "$RELAY" \
  --public "$RELAY" \
  --relay-token env:RELAY_TOKEN \
  "${SECRET_ARGS[@]}"
a2a serve run --dir "$WORK/reviewer" --quiet --detach

echo "--- coder status ---"
a2a serve status --dir "$WORK/coder"
echo "--- coder doctor --online ---"
a2a serve doctor --online --dir "$WORK/coder"
echo "--- reviewer status ---"
a2a serve status --dir "$WORK/reviewer"
echo "--- in-process sdk/meshnet state files (no agentmesh-node sidecar) ---"
ls -1 "$WORK/coder/.a2a/state/agentmesh"
echo "--- running Docker sandboxes ---"
docker ps --filter "label=a2a.agent" --format "table {{.Names}}\t{{.Image}}\t{{.Status}}"

CODER_LOCAL="$(a2a serve url --dir "$WORK/coder" --local)"
CODER_PUBLIC="$(a2a serve url --dir "$WORK/coder" --public)"

banner "3. Discovering reviewer over the Agent Mesh DHT & delegating a live LLM task (coder -> reviewer)"
for _ in $(seq 1 30); do
  PEERS_OUT="$(a2a serve peers --dir "$WORK/coder")"
  if grep -q "reviewer" <<<"$PEERS_OUT"; then
    break
  fi
  sleep 0.5
done
echo "$PEERS_OUT"
grep -q "reviewer" <<<"$PEERS_OUT"

INVITE_OUT="$(a2a serve pair invite coder --dir "$WORK/reviewer" -o json)"
REVIEWER_CODE="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["code"])' <<<"$INVITE_OUT")"

a2a serve contact add reviewer --dir "$WORK/coder" --code "$REVIEWER_CODE"
a2a serve contact list --dir "$WORK/coder"

echo "Expected token inside reviewer container NOTE.txt: ${REVIEW_TOKEN}"
DELEGATED_REPLY="$(a2a send -a "$CODER_LOCAL" \
  "Ask the reviewer agent synchronously (without --async) to read NOTE.txt in its current working directory and return the exact token inside, then reply with that exact token.")"
echo "coder reply: $DELEGATED_REPLY"
grep -Fq "$REVIEW_TOKEN" <<<"$DELEGATED_REPLY"

banner "4. Calling coder from outside the relay via public /ingress/ URL (a2a serve tokens add)"
echo "Public A2A URL: $CODER_PUBLIC"
echo "--- Public AgentCard (rewritten to /ingress/<peer_id>/a2a/coder) ---"
CARD_OUT="$(HOME="$EXTERNAL_HOME" a2a card get -a "$CODER_PUBLIC")"
echo "$CARD_OUT"
grep -Fq "$CODER_PUBLIC" <<<"$CARD_OUT"

echo "--- Verifying unauthenticated external request is rejected (401) ---"
if HOME="$EXTERNAL_HOME" a2a send -a "$CODER_PUBLIC" "hello" >/dev/null 2>&1; then
  echo "error: unauthenticated external call unexpectedly succeeded" >&2
  exit 1
fi
echo "Unauthenticated external call rejected as expected."

EXT_INVITE="$(a2a serve pair invite external --dir "$WORK/coder" -o json)"
EXT_CODE="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["code"])' <<<"$EXT_INVITE")"

HOME="$EXTERNAL_HOME" a2a serve tokens add "$CODER_PUBLIC" "$EXT_CODE"
echo "Expected token inside reviewer container PUBLIC_CHECK.txt: ${EXT_TOKEN}"
EXT_REPLY="$(HOME="$EXTERNAL_HOME" a2a send -a "$CODER_PUBLIC" \
  "Ask the reviewer agent synchronously (without --async) to read PUBLIC_CHECK.txt in its current working directory and return the exact token inside, then reply with that exact token.")"
echo "public ingress -> coder -> reviewer reply: $EXT_REPLY"
grep -Fq "$EXT_TOKEN" <<<"$EXT_REPLY"

a2a serve stop --dir "$WORK/coder" >/dev/null
a2a serve stop --dir "$WORK/reviewer" >/dev/null
kill "$HUB_PID" >/dev/null 2>&1 || true
wait "$HUB_PID" 2>/dev/null || true
unset HUB_PID RELAY_TOKEN

banner "5. Starting local RS256 OIDC Device-Flow Identity Provider & OIDC-enabled agentmesh-one relay"
cat >"$WORK/idp/main.go" <<'EOF'
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	issuer := "http://" + ln.Addr().String()
	email := os.Args[1]

	var (
		mu       sync.Mutex
		approved = map[string]bool{}
		seq      atomic.Int64
	)

	signJWT := func(claims map[string]any) (string, error) {
		hdr, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
		if err != nil {
			return "", err
		}
		pay, err := json.Marshal(claims)
		if err != nil {
			return "", err
		}
		signing := base64.RawURLEncoding.EncodeToString(hdr) + "." + base64.RawURLEncoding.EncodeToString(pay)
		sum := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
		if err != nil {
			return "", err
		}
		return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           issuer,
			"jwks_uri":                         issuer + "/jwks",
			"authorization_endpoint":           issuer + "/authorize",
			"device_authorization_endpoint":    issuer + "/device",
			"token_endpoint":                   issuer + "/token",
			"response_types_supported":         []string{"code"},
			"subject_types_supported":          []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"alg": "RS256",
				"use": "sig",
				"kid": "k1",
				"n":   base64.RawURLEncoding.EncodeToString(priv.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.PublicKey.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("POST /device", func(w http.ResponseWriter, _ *http.Request) {
		n := seq.Add(1)
		dc := fmt.Sprintf("dc-%d", n)
		uc := fmt.Sprintf("USER-%04d", n)
		mu.Lock()
		approved[dc] = false
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               dc,
			"user_code":                 uc,
			"verification_uri":          issuer + "/activate",
			"verification_uri_complete": issuer + "/activate?code=" + dc,
			"interval":                  1,
			"expires_in":                300,
		})
	})
	mux.HandleFunc("GET /activate", func(w http.ResponseWriter, r *http.Request) {
		dc := r.URL.Query().Get("code")
		mu.Lock()
		_, ok := approved[dc]
		if ok {
			approved[dc] = true
		}
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown device code", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, "approved %s for %s\n", dc, email)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		dc := r.FormValue("device_code")
		aud := r.FormValue("client_id")
		if aud == "" {
			aud = "agentmesh"
		}
		mu.Lock()
		ready, ok := approved[dc]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired_token"})
			return
		}
		if !ready {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		now := time.Now()
		tok, err := signJWT(map[string]any{
			"iss":            issuer,
			"sub":            "user-ben-1",
			"aud":            aud,
			"email":          email,
			"email_verified": true,
			"iat":            now.Unix(),
			"exp":            now.Add(time.Hour).Unix(),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
	})

	if err := os.WriteFile(os.Args[2], []byte(issuer), 0o600); err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.Serve(ln, mux))
}
EOF

CGO_ENABLED=0 go build -o "$BIN_DIR/test-oidc-idp" "$WORK/idp/main.go"
"$BIN_DIR/test-oidc-idp" "Ben@example.com" "$WORK/idp/url" >"$WORK/idp/idp.log" 2>&1 &
IDP_PID=$!

for _ in $(seq 1 50); do
  if [[ -s "$WORK/idp/url" ]]; then
    break
  fi
  sleep 0.1
done
IDP_URL="$(cat "$WORK/idp/url")"
curl -fsS "${IDP_URL}/.well-known/openid-configuration" >/dev/null
echo "Local OIDC Device-Flow Identity Provider ready at: ${IDP_URL}"

OIDC_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
OIDC_RELAY="http://127.0.0.1:${OIDC_PORT}"
"$AGENTMESH_ONE_BIN" \
  --bind-address 127.0.0.1 \
  --port "$OIDC_PORT" \
  --data-dir "$WORK/oidc-hub" \
  --policy-file "$WORK/oidc-hub/policy.json" \
  --issuer "$IDP_URL" \
  --oidc-client-id agentmesh \
  --allowed-audiences agentmesh \
  --no-join-token \
  >"$WORK/oidc-hub/agentmesh-one.log" 2>&1 &
OIDC_HUB_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "${OIDC_RELAY}/readyz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl -fsS "${OIDC_RELAY}/readyz" >/dev/null
echo "OIDC-enabled Agent Mesh relay ready at: ${OIDC_RELAY}"

approve_device_signin() {
  local agent_dir="$1"
  local log_path="$agent_dir/.a2a/state/server.log"
  for _ in $(seq 1 300); do
    if [[ -f "$log_path" ]]; then
      local link
      link="$(sed -n 's/^→ sign in to the relay .*: \(https\{0,1\}:\/\/[^[:space:]]*\).*/\1/p' "$log_path" | tail -n1)"
      if [[ -n "$link" ]]; then
        curl -fsS "$link" >/dev/null
        return 0
      fi
    fi
    sleep 0.2
  done
  echo "error: timed out waiting for device sign-in link in $log_path" >&2
  "$BIN_DIR/a2a-serve" stop --dir "$agent_dir" >/dev/null 2>&1 || true
  return 1
}

banner "6. Signing in oidc-coder & oidc-reviewer as Ben@example.com (no --relay-token) & delegating without pairing code"
approve_device_signin "$WORK/oidc-coder" &
SIGNIN_PID=$!
a2a serve run opencode --dir "$WORK/oidc-coder" \
  --name oidc-coder \
  --description "OIDC-authenticated coding agent" \
  "${IMAGE_ARGS[@]}" \
  --allow-tools read,search,execute \
  --private "$OIDC_RELAY" \
  --public "$OIDC_RELAY" \
  "${SECRET_ARGS[@]}" \
  --quiet \
  --detach
wait "$SIGNIN_PID"
unset SIGNIN_PID

approve_device_signin "$WORK/oidc-reviewer" &
SIGNIN_PID=$!
a2a serve run opencode --dir "$WORK/oidc-reviewer" \
  --name oidc-reviewer \
  --description "OIDC-authenticated review agent" \
  "${IMAGE_ARGS[@]}" \
  --allow-tools read,search \
  --private "$OIDC_RELAY" \
  --public "$OIDC_RELAY" \
  "${SECRET_ARGS[@]}" \
  --quiet \
  --detach
wait "$SIGNIN_PID"
unset SIGNIN_PID

echo "--- oidc-coder owner.json ---"
cat "$WORK/oidc-coder/.a2a/state/agentmesh/owner.json"
echo
grep -Fq '"ben@example.com"' "$WORK/oidc-coder/.a2a/state/agentmesh/owner.json"

for _ in $(seq 1 30); do
  OIDC_PEERS="$(a2a serve peers --dir "$WORK/oidc-coder")"
  if grep -q "oidc-reviewer" <<<"$OIDC_PEERS"; then
    break
  fi
  sleep 0.5
done
echo "$OIDC_PEERS"
grep -q "oidc-reviewer" <<<"$OIDC_PEERS"

echo "--- Adding oidc-reviewer contact WITHOUT --code (same-owner OIDC Biscuit identity) ---"
a2a serve contact add oidc-reviewer --dir "$WORK/oidc-coder"
a2a serve contact list --dir "$WORK/oidc-coder"

OIDC_CODER_LOCAL="$(a2a serve url --dir "$WORK/oidc-coder" --local)"
echo "Expected token inside oidc-reviewer container OIDC_NOTE.txt: ${OIDC_TOKEN}"
OIDC_REPLY="$(a2a send -a "$OIDC_CODER_LOCAL" \
  "Ask the oidc-reviewer agent synchronously (without --async) to read OIDC_NOTE.txt in its current working directory and return the exact token inside, then reply with that exact token.")"
echo "oidc-coder -> oidc-reviewer reply: $OIDC_REPLY"
grep -Fq "$OIDC_TOKEN" <<<"$OIDC_REPLY"

echo "--- Restarting oidc-coder to verify stored Biscuit + owner.json reuse (no second sign-in) ---"
a2a serve restart --dir "$WORK/oidc-coder"
a2a serve status --dir "$WORK/oidc-coder"

banner "ALL REAL E2E CHECKS PASSED (Token Join + Cloudflare Public Ingress + OIDC Device Sign-In + Same-Owner Zero-Code Delegation)"
