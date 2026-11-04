# Lesson 3 — Agent Mesh relay (experimental)

Lesson 2 put an agent on a tailnet. This lesson puts two agents on an
[Agent Mesh](https://google.github.io/agentmesh/) relay: no Tailscale
account, agents reachable from behind NAT, and a public URL for each on the
relay. a2a-serve runs Agent Mesh's `meshnet` library in-process, so there is
no sidecar to run.

## What you'll learn

- How to run a relay (`agentmesh-one`) on your machine
- How an agent joins it with `--private <relay-url> --public <relay-url>`
- How agents on the relay find each other and call each other
- How a caller outside the relay pairs with an agent at its public URL

## Prerequisites

- `a2a-serve` built with Agent Mesh support, and the `a2a` CLI (see
  [Getting started](../../docs/getting-started.md#install)):

  ```bash
  go install -tags agentmesh github.com/a2aproject/a2a-serve@latest
  ```

- Docker, and an API key for the agent, as in lesson 1
- The relay, at the Agent Mesh version this a2a-serve joins with:

  ```bash
  go install github.com/google/agentmesh/cmd/agentmesh-one@v0.2.0
  ```

## 1. Run the relay

The relay's policy decides what agents may do. This one lets agents publish
A2A services and get public URLs that forward requests unchanged
(`PASSTHROUGH`), so a2a-serve's own pairing codes work there:

```bash
mkdir -p ~/a2a-lesson3/relay
cat > ~/a2a-lesson3/relay/policy.json <<'JSON'
{
  "roles": [
    {"name": "mesh-admin", "allowed_services": ["*"], "allowed_targets": ["*"], "allowed_ingress": [{"services": ["*"]}]},
    {"name": "mesh:role:router", "allowed_services": ["*"], "allowed_targets": ["*"]},
    {
      "name": "mesh:role:node",
      "allowed_services": ["*"], "allowed_targets": ["*"], "allowed_publish": ["*"], "allowed_labels": ["*"],
      "allowed_ingress": [{"services": ["a2a://*"], "allowed_auth_modes": ["INGRESS_AUTH_MODE_PASSTHROUGH"]}]
    }
  ]
}
JSON
agentmesh-one --bind-address 127.0.0.1 --port 8443 \
  --data-dir ~/a2a-lesson3/relay --policy-file ~/a2a-lesson3/relay/policy.json
```

The relay serves its control plane, the connections between agents and the
public URLs on one port, and writes a join token to
`~/a2a-lesson3/relay/join-token`. In another terminal:

```bash
export RELAY=http://127.0.0.1:8443
export RELAY_TOKEN="$(cat ~/a2a-lesson3/relay/join-token)"
```

A relay that people sign in to with their accounts prints a sign-in link on
an agent's first run, as Tailscale does. This local relay has no identity
provider, so the agents join with the token.

## 2. Join two agents

```bash
mkdir -p ~/a2a-lesson3/coder && cd ~/a2a-lesson3/coder
a2a-serve config set --agent opencode --name coder --secret OPENROUTER_API_KEY \
  --private "$RELAY" --public "$RELAY" --relay-token env:RELAY_TOKEN
a2a-serve start --detach

mkdir -p ~/a2a-lesson3/reviewer && cd ~/a2a-lesson3/reviewer
a2a-serve config set --agent opencode --name reviewer --secret OPENROUTER_API_KEY \
  --private "$RELAY" --public "$RELAY" --relay-token env:RELAY_TOKEN
a2a-serve start --detach
```

`--relay-token` is a `config set` setting, so `config set --agent` creates
each agent with it before its first start, and `start` starts it.

```bash
cd ~/a2a-lesson3/coder && a2a-serve status
```

```
coder (opencode) running for 4s, pid 51200
  dir      /home/you/a2a-lesson3/coder
  local    http://127.0.0.1:41823
  private  http://agentmesh.local/mesh/12D3KooW…/a2a/coder
           (works only from other a2a-serve agents on the relay; give people the public URL)
  public   http://127.0.0.1:8443/ingress/12D3KooW…/a2a/coder
```

## 3. Let coder call reviewer

Agents on the relay find each other by name:

```bash
a2a-serve peers --dir ~/a2a-lesson3/coder     # lists reviewer
```

Only agents with the join token are on this relay, so everyone on it can
call reviewer, and coder needs no code:

```bash
a2a-serve contact add reviewer --dir ~/a2a-lesson3/coder
a2a send -a "$(a2a-serve status --url=local --dir ~/a2a-lesson3/coder)" \
  "Ask reviewer what it thinks of the README of this project."
```

coder's call goes through its server, over the relay, to reviewer. No
credentials enter coder's sandbox.

## 4. Call an agent from outside the relay

The public URL forwards requests unchanged, so a caller pairs there like
anywhere else:

```bash
PUBLIC="$(a2a-serve status --url=public --dir ~/a2a-lesson3/coder)"
a2a-serve pair invite me --dir ~/a2a-lesson3/coder     # prints a code
a2a-serve pair join "$PUBLIC" <code>
a2a send -a "$PUBLIC" "What does this project do?"
```

## 5. Clean up

```bash
a2a-serve stop --dir ~/a2a-lesson3/coder
a2a-serve stop --dir ~/a2a-lesson3/reviewer
# stop agentmesh-one with Ctrl+C
rm -rf ~/a2a-lesson3
```

## Next steps

- Sign in with your account: a relay with an identity provider prints a
  sign-in link on the first run. Your agents call each other without a code,
  and other people's agents need one, because the identity provider may
  accept anyone with an account.
- Run the relay somewhere with a stable address; agents and pairing tokens
  are tied to it.
