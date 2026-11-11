# Agent Mesh

This page explains how a2a-serve uses Agent Mesh, a zero-trust network for agents, and how a relay works.

> [!NOTE]
> Experimental. Joining a relay with `--private <relay-url>` and `--public <relay-url>` works today in builds with `-tags agentmesh`, and `a2a-serve relay run` runs a relay. Egress through the relay and the directory of advertised nodes are planned.

[Agent Mesh](https://google.github.io/agentmesh/) lets nodes enroll with a control plane, get short-lived credentials bound to their own keys, reach each other through routers from behind NAT, and be reached from the internet through ingress.

a2a-serve decides who may do what with an agent. Agent Mesh decides who is on the network and proves who each caller is. The server writes everything Agent Mesh needs from its own settings, so users never edit Agent Mesh configuration.

| a2a-serve | Agent Mesh |
|---|---|
| The agent, its sandbox and approvals | The network between agents: routers, relaying, discovery |
| Pairing, contacts and the access policy | Identity: enrollment, node keys, credentials |
| Messengers: Telegram, Discord, email | Public ingress and egress |

![a2a-serve on an Agent Mesh relay](../graphs/agentmesh.png#gh-light-mode-only)
![a2a-serve on an Agent Mesh relay](../graphs/agentmesh-dark.png#gh-dark-mode-only)

## Joining a relay

[Agent Mesh relays](../guide/networks.md#agent-mesh-relays-experimental) shows the commands. The server runs an Agent Mesh node inside its own process, with no sidecar. The node's key stays in the agent directory, so the agent keeps its URLs across restarts and backups.

The agent enrolls by signing in to the relay, or with an invite from `a2a-serve relay invite`. It then publishes the A2A service `a2a://<name>`, which other agents on the relay find in the directory. With `--public <relay-url>`, the relay also gives the service a public URL. Ingress forwards the `Authorization` header unchanged, so pairing tokens and Google sign-in work there.

The [Agent Mesh relay example](../../examples/03-agentmesh-relay/README.md) runs a relay and two agents on one machine.

## How the access model maps

The [access model](access-model.md) has the same entities on every network. On a relay:

| Access model | Agent Mesh |
|---|---|
| Agent | a node and its `a2a://<name>` service |
| Private network | everyone the relay admits: enrolled nodes and signed-in people |
| Public network | ingress for `a2a://<name>` |
| Identity | the node the relay proves, and the verified email of a signed-in person |
| `self` | a caller whose verified email equals the one saved when this agent signed in |
| `private` | any caller the relay admits |
| Pairing | a server credential that the relay forwards unchanged through ingress |
| Contact | an agent this one calls through the relay, by directory name or URL |

Every request passes three steps:

1. **Agent Mesh admits.** Only enrolled nodes and ingress reach the agent. The relay decides who may enroll and publish.
2. **The server resolves.** It builds the caller's identity and gives it a [role](../guide/identities.md#roles): `owner`, `member` or `guest`.
3. **The server acts.** The role decides what the caller may do with tasks and approvals.

A relay admits like a tailnet: everyone it admits can call every other agent on it, so `--private-allow-incoming` defaults to `everyone`. `--private-allow-incoming paired` narrows step 2 to you and your pairings. To remove someone from the network, use `a2a-serve relay remove` at the relay, which bans their nodes.

## Calls between agents

On a relay the server dials contacts through its node, so the callee sees this agent's node identity, and no credential enters the sandbox. The callee gives a call that came through another agent at most the member role, whoever started it.

Planned: advertised webhook nodes become directory entries. Ben's agent publishes `a2a://grok@ben`, and Anna's agent finds it with `peers` and adds it with `contact add grok@ben`. Calls still go through Ben's agent, which keeps the webhook's credentials and checks Anna's agent against its own policy. Likewise, the sandbox will reach the network through Agent Mesh egress, so the relay's policy decides which hosts an agent may reach.

## Running a relay

`a2a-serve relay run` runs a relay as one process, the front. The front serves the relay's port and runs `agentmesh-one`, at the version this a2a-serve pins, as a child process on loopback. It serves the relay's control API and identity provider itself and passes the rest to `agentmesh-one`. At every start the server sets all `agentmesh-one` flags and applies the mesh policy, so operators never configure Agent Mesh directly. [Relays](../guide/relays.md) shows how to run one, and [HTTP Routes](../reference/http-routes.md#relay-routes) lists the front's routes.

### Sign-in

Agent Mesh v0.2.0 can't use Google's device flow and admits anyone its identity provider accepts. So the front is the relay's identity provider:

1. The person signs in with the relay's Google app.
2. The front checks the email against the allowlist.
3. The front issues the ID token that Agent Mesh enrolls the agent with.

Each renewal checks the account again, and the relay bans the agents of people the allowlist no longer admits. `agentmesh-one` trusts no other issuer, and signed-in accounts can't mint join tokens.

Source builds need `-tags agentmesh` to join a relay. It makes the binary much larger, so release binaries leave it out until `meshnet` is its own Go module. The relay commands don't need it.
