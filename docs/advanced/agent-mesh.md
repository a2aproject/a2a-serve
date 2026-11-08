# Agent Mesh

> [!NOTE]
> Experimental. Joining a relay with `--private <relay-url>` and `--public <relay-url>` works today in builds with `-tags agentmesh`, and `a2a-serve relay run` runs a relay. Egress through the relay and the directory of advertised nodes are planned, and marked as such below.

[Agent Mesh](https://google.github.io/agentmesh/) is a zero-trust network for agents: nodes enroll with a control plane, get short-lived credentials bound to their own keys, reach each other through routers from behind NAT, and can be reached from the internet through ingress. a2a-serve runs on top of it.

The boundary between them:

| a2a-serve | Agent Mesh |
|---|---|
| The agent and its ACP executor | The network between agents: routers, relaying, discovery |
| The Docker sandbox | Identity: enrollment, node keys, credentials |
| Pairing, contacts and the access policy | Public ingress |
| Approvals | Egress to the internet |
| Messengers: Telegram, Discord, email | |

a2a-serve decides who may do what with an agent. Agent Mesh decides who is on the network and proves who each caller is. a2a-serve writes everything Agent Mesh needs from its own settings, so users never edit Agent Mesh configuration.

![a2a-serve on an Agent Mesh relay](../graphs/agentmesh.png#gh-light-mode-only)
![a2a-serve on an Agent Mesh relay](../graphs/agentmesh-dark.png#gh-dark-mode-only)

## Joining a relay

[Agent Mesh relays](../guide/networks.md#agent-mesh-relays-experimental) shows the commands. How it works:

- a2a-serve runs a `meshnet` node inside its own process, with no sidecar. The node's key and peer ID are kept in `.a2a/state/agentmesh`, so it keeps its URLs across restarts and backups.
- It enrolls by signing in to the relay's identity provider, or with a join token (`--relay-token`), such as an invite from `a2a-serve relay invite`.
- It publishes the A2A service `a2a://<name>`. Other agents on the relay find it in the directory and dial it with their own node identity.
- With `--public <relay-url>`, the relay also gives `a2a://<name>` a public URL. Ingress runs in `passthrough` mode and forwards the `Authorization` header unchanged, so pairing tokens and Google sign-in work there.

The [Agent Mesh relay example](../../examples/03-agentmesh-relay/README.md) runs a relay and two agents on one machine.

## How the access model maps

The [access model](access-model.md) has the same entities on every network. On a relay:

| Access model | Agent Mesh |
|---|---|
| Agent | a `meshnet` node and its `a2a://<name>` service |
| Private network | everyone the relay admits: enrolled nodes and signed-in people |
| Public network | ingress for `a2a://<name>` in `passthrough` mode |
| Identity | `meshnet.WhoIs` returns `CallerInfo`: the peer ID becomes the node, and the principal becomes the email only when it is a verified email |
| `self` | a caller whose verified email equals the one saved when this agent signed in |
| `private` | any caller the relay admits |
| Pairing | an a2a-serve credential; the relay forwards it unchanged through ingress |
| Contact | an agent this one calls through `meshnet`, by directory name or URL |

Every request passes three steps:

1. **Agent Mesh admits.** The router delivers only requests from enrolled nodes, or from ingress. The relay's policy decides who may enroll and publish.
2. **a2a-serve resolves.** It builds the caller's identity from `WhoIs`, or from a pairing token or sign-in on the public URL, and gives it a [role](../guide/identities.md#roles): `owner`, `member` or `guest`.
3. **a2a-serve acts.** The role decides what the caller may do with tasks and approvals.

`--private-allow-incoming paired` narrows step 2 to you and your pairings while the relay still admits everyone else to the network. Removing someone from the private network happens at the relay, with `a2a-serve relay remove`, which bans their nodes.

A relay admits like a tailnet: everyone it admits can call everyone else it admits, so `--private-allow-incoming` defaults to `everyone` on every relay. The relay decides who it admits: people who sign in with an email on its allowlist, and agents it invites.

## Calls between agents

The agent calls other agents with the `a2a` CLI, through the server. On a relay the server dials contacts through `meshnet`, so the callee sees this agent's node identity, and no credential enters the sandbox. The callee gives a call that came through another agent at most the member role, whoever started it.

Advertised webhook nodes become directory entries: Ben's agent publishes `a2a://grok@ben`, and Anna's agent finds it with `peers` and adds it with `contact add grok@ben`. Calls still go through Ben's agent, which keeps the webhook's credentials and checks Anna's agent against its own policy (planned; on a tailnet, advertisements spread by gossip today).

## Egress

The sandbox reaches the network only through the server (planned). On a relay, the server sends that traffic through Agent Mesh egress, so the relay's policy decides which hosts an agent may reach, and keys for inference or shared MCP servers can stay on the relay (`egress://`, `inference://`, `mcp://`; planned).

## Running a relay

`a2a-serve relay run` runs a relay as one a2a-serve process, the front. It serves the relay's port and runs `agentmesh-one`, at the version this a2a-serve pins, as a child process on loopback. The front:

- reports the relay's versions at `/.well-known/a2a-serve-relay`;
- serves the relay's control API to callers with the admin token;
- serves the relay's identity provider, when people may sign in;
- blocks Agent Mesh's admin API, console and metrics, which a2a-serve manages or doesn't use;
- proxies everything else to `agentmesh-one`, WebSockets included.

a2a-serve sets every `agentmesh-one` flag and applies the mesh policy through the admin API at every start. The policy lets every node call, publish and expose A2A services, with public URLs in `passthrough` mode. `agentmesh-one` runs with no standing join token, so agents join by sign-in or with an invite, a single-use bootstrap token with autonomous recovery.

a2a-serve is the relay's identity provider. Agent Mesh v0.2.0 can't use Google's device flow, and admits anyone its identity provider accepts. So the front runs the device flow itself: the person signs in with the relay's Google app, the front checks the email against the allowlist, and then issues the ID token that Agent Mesh enrolls the agent with. The agent keeps a refresh token, and each renewal checks the account with Google and against the allowlist again. Agent Mesh also renews an enrolled agent on its own until the agent's session ends, so at every start the relay bans the agents of people its allowlist no longer admits, and lifts the bans of people it admits. `agentmesh-one` trusts no other issuer. It treats this one as a workload issuer, so its user API refuses signed-in accounts, which keeps them from minting join tokens. [HTTP Routes](../reference/http-routes.md#relay-routes) lists the front's routes, and [Relays](../guide/relays.md) shows how to run one.

Source builds need `-tags agentmesh` to join a relay. It adds about 36 MB to the binary, so release binaries leave it out until `meshnet` is its own Go module. The relay commands don't need it. `doctor` checks that the build supports relays and, with `--online`, that the relay answers with the pinned Agent Mesh version.
