# a2a-serve on Agent Mesh

> [!NOTE]
> Experimental. Joining a relay with `--private <relay-url>` and `--public <relay-url>` works today in builds with `-tags agentmesh`. The access flags, verified-email mapping and egress through the relay are planned, and marked as such below.

[Agent Mesh](https://google.github.io/agentmesh/) is a zero-trust network for agents: nodes enroll with a control plane, get short-lived credentials bound to their own keys, reach each other through routers from behind NAT, and can be reached from the internet through ingress. a2a-serve runs on top of it.

The boundary between them:

| a2a-serve | Agent Mesh |
|---|---|
| The harness and its ACP executor | The network between agents: routers, relaying, discovery |
| The Docker sandbox | Identity: enrollment, node keys, credentials |
| Pairing, contacts and the access policy | Public ingress |
| Approvals | Egress to the internet |
| Messengers: Telegram, Discord, email | |

a2a-serve decides who may do what with an agent. Agent Mesh decides who is on the network and proves who each caller is. a2a-serve writes everything Agent Mesh needs from its own settings, so users never edit Agent Mesh configuration.

![a2a-serve on Agent Mesh](../graphs/agentmesh.png#gh-light-mode-only)
![a2a-serve on Agent Mesh](../graphs/agentmesh-dark.png#gh-dark-mode-only)

## Joining a relay

```bash
a2a-serve start opencode --private https://relay.example.com --public https://relay.example.com
# first run: → sign in to the relay https://relay.example.com: https://…
a2a-serve config set --relay-token env:RELAY_TOKEN    # servers: join with a token, no sign-in
```

- The agent is a `meshnet` node running inside the a2a-serve process, with no sidecar. Its key and peer ID are kept in `.a2a/state/agentmesh`, so it keeps its URLs across restarts and backups.
- It enrolls by signing in to the relay's identity provider, or with a join token (`--relay-token`).
- It publishes the A2A service `a2a://<name>`. Other agents on the relay find it in the directory and dial it with their own node identity.
- With `--public <relay-url>`, the relay also gives `a2a://<name>` a public URL. Ingress runs in `passthrough` mode and forwards the `Authorization` header unchanged, so pairing tokens and Google sign-in work there.

[Lesson 3](../examples/03-agentmesh-relay/README.md) runs a relay and two agents on one machine.

## How the access model maps

The [access model](access-model.md) has the same entities on every network. On a relay:

| Access model | Agent Mesh |
|---|---|
| Agent | a `meshnet` node and its `a2a://<name>` service |
| Private network | everyone the relay admits: enrolled nodes and signed-in people |
| Public network | ingress for `a2a://<name>` in `passthrough` mode |
| Identity | `meshnet.WhoIs` returns `CallerInfo`: the peer ID becomes the node, and the principal becomes the email only when it is a verified email (planned) |
| `self` | a caller whose verified email equals the one saved when this agent signed in |
| `private` | any caller the relay admits |
| Pairing | an a2a-serve credential; the relay forwards it unchanged through ingress |
| Contact | an agent this one calls through `meshnet`, by directory name or URL |

Every request passes three steps:

1. **Agent Mesh admits.** The router delivers only requests from enrolled nodes, or from ingress. The relay's policy decides who may enroll and publish.
2. **a2a-serve resolves.** It builds the caller's identity from `WhoIs`, or from a pairing token or sign-in on the public URL, and gives it a role: `owner`, `authenticated` or `anonymous`.
3. **a2a-serve acts.** The role decides what the caller may do with tasks and approvals.

`--private-access paired` narrows step 2 to you and your pairings while the relay still admits everyone else to the network (planned). Removing someone from the private network happens at the relay, by revoking their enrollment.

A relay whose sign-in accepts any account of a public identity provider admits anyone with such an account. On such a relay `--private-access` defaults to `paired` until you set it (planned).

## Calls between agents

The harness calls other agents with the `a2a` CLI, through the server. On a relay the server dials contacts through `meshnet`, so the callee sees this agent's node identity, and no credential enters the sandbox. The callee gives a call that came through another agent at most `authenticated`, whoever started it.

Advertised webhook nodes become directory entries: Ben's agent publishes `a2a://grok@ben`, and Anna's agent finds it with `peers` and adds it with `contact add grok@ben`. Calls still go through Ben's agent, which keeps the webhook's credentials and checks Anna's agent against its own policy (planned; on a tailnet, advertisements spread by gossip today).

## Egress

The sandbox reaches the network only through the server (planned). On a relay, the server sends that traffic through Agent Mesh egress, so the relay's policy decides which hosts an agent may reach, and keys for inference or shared MCP servers can stay on the relay (`egress://`, `inference://`, `mcp://`; planned).

## Running a relay

The relay is `agentmesh-one` at the version this a2a-serve joins with (v0.2.0): one binary that serves the control plane, routers and ingress on one HTTPS port, with SQLite in a data directory. On a VM with a disk it needs nothing else. On Cloud Run it runs as a single instance with Postgres, since the filesystem is wiped on every new revision. `--tunnel cloudflare` runs it on a laptop for demos, but the URL changes on every restart.

Source builds need `-tags agentmesh`. It adds about 36 MB to the binary, so release binaries leave it out until `meshnet` is its own Go module. `doctor` checks that the build supports relays and, with `--online`, that the relay answers.

## Asks for Agent Mesh

| Ask | Why |
|---|---|
| A verified `Email` field in `CallerInfo` | `self` and `--allow-email` need an email the relay attests, kept apart from free-form principals |
| Per-request attenuated credentials | A call made on behalf of a user should carry no more than that call needs |
| Narrow node-minted bootstrap tokens | `pair invite` could hand out a token that enrolls one node for one service |
| Hybrid `edge` and `passthrough` ingress | One public URL for OAuth callers verified by the relay and for pairing tokens checked by a2a-serve |
| `meshnet` as its own Go module | Embedding it today pulls libp2p, envoy and biscuit-go into every build |
| An exported hub package | `agentmesh-one` lives under `internal/`, so a2a-serve can only run the binary or image |
| A custom dialer for `meshnet` | It would let `meshnet` run over tsnet, putting a relay inside a tailnet |
| Version compatibility before 1.0 | a2a-serve pins one `meshnet` version and has to know which relays it can join |
| Stable ingress names | Public URLs carry 52-character peer IDs |
