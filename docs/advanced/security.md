# Security

An a2a-serve agent can read your project, run commands and reach the network, and other people and agents can give it work. This page describes what the current version protects and what it doesn't yet. Report vulnerabilities as described in [SECURITY.md](../../SECURITY.md).

## The sandbox

By default the agent runs in Docker with:

- the workspace and the `--mount` directories, and nothing else from your disk. `.a2a/` appears as an empty read-only directory, so the agent can't read its own secrets, tokens, database or config. The `.a2a/` directories of other agents in the mounts appear empty too, which keeps their secrets and control sockets out of reach. The server refuses a workspace or mount that is your home directory, `/` or a directory that holds it, unless `--allow-broad-mount` names it. `.mcp.json` appears as a read-only file with no servers, so the agent gets only the MCP servers the server passes it;
- `state/home` as its home directory, where it keeps its sessions and config;
- your user ID (container root under rootless Docker, which maps to you), all capabilities dropped, `no-new-privileges`, and limits of 4 GB of memory, 2 CPUs and 512 processes;
- a socket to the server, through which it calls other agents. The server makes those calls with its own credentials, so contact tokens never enter the sandbox. Each container gets a socket of its own in a read-only directory, and the socket serves only the conversations of that container. On Docker Desktop the socket is a TCP port on the host's loopback, and each container sends a key of its own.

Each authenticated user gets a container of their own, with a home in `state/homes`, so their conversations can't read the history of other users' conversations. [Anonymous callers](../guide/identities.md#anonymous-callers) share one more container, with `state/home-anonymous` as its home and no socket to the server. All containers share the workspace, the `--mount` directories and the `--secret` values.

Every tool call that changes something waits for approval from the person who started the task, or an owner (see [Approvals](../guide/approvals.md)). The agent can't answer the approvals of agents it calls; the server relays them to the user.

What the sandbox doesn't protect yet:

- **API keys are readable inside it.** Values passed with `--secret` are environment variables of the agent, so a tricked agent can read them and send them anywhere ([#16](https://github.com/a2aproject/a2a-serve/issues/16)).
- **It can reach any host.** A tricked agent can send the files it can read anywhere ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **It reaches the tailnet as you.** The tailnet is a trusted private network. When the machine running the agent is on it, the agent's connections to tailnet addresses go out through that machine's Tailscale node, so this agent's private URL, your other agents and other tailnet services see your device and grant your rights, whoever started the task.
- **The workspace is writable** unless mounted `:ro`, including `.git` and skills. A change the agent makes there applies to the next conversation, and to you when you run tools in the project.
- **An agent created in the mounts of a running agent is visible to its running containers.** `a2a-serve start` refuses to create an agent inside the directory of a running agent, and the server refuses new containers once another agent appears in their mounts. Containers that already run see the new agent's `.a2a/` until `a2a-serve restart`.
- **Docker is the boundary.** Kernel and Docker escapes are out of scope for this project.

`--sandbox local` runs the agent on the host, as you, with no isolation. It can read everything you can, including `~/.config/a2a-serve/credentials.json` with your owner token, the `.a2a/` directories of your other agents and the conversations of every user. Use it only for agents and callers you trust completely.

## Who can call the agent

Every A2A request needs a known caller. These routes are public: the agent card, `/pair` (which redeems codes), `/skill.md`, `/download/…` and `/hitl/<token>`.

The server limits failed attempts to redeem a code for each caller, on `/pair` and with `join <code>` in a chat. Through Funnel and the relay, it tells callers apart by their own address or relay identity. Anonymous callers through the relay, and callers over their limit, share one short backoff. A valid code from an address over its limit goes through after that backoff. `pair join` saves a node token only if it is for a node of the agent it joined.

The public routes also limit the open requests of each caller and the size and send time of their bodies. This keeps capacity free for callers who authenticate ([request limits](../reference/http-routes.md#request-limits)).

A web page open in your browser can't make requests that change state on the agent, because the server refuses cross-site requests and needs a JSON body ([browser requests](../reference/http-routes.md#browser-requests)).

| Network | Who gets in |
| --- | --- |
| localhost | Callers with a token. On start, the server saves a token of the built-in `local` pairing for the user running it, which works only on the local URL. The server removes the saved token when it stops, so a program that later listens on the same port isn't sent it. A loopback connection alone grants nothing, because tunnels also reach the server from loopback. |
| Tailscale | Everyone on the tailnet, or with `--private-access paired` only callers with a token. Your own untagged devices, identified by Tailscale as your user, are owners. The tailnet's admin console and access controls decide who is on it. |
| Agent Mesh relay | Everyone the relay admits, or with `--private-access paired` only callers with a token. The relay's enrollment decides who is on it ([who a relay admits](agent-mesh.md#how-the-access-model-maps)). |
| Public URL (Funnel, relay) | Callers with a token, including people on the `--allow-email` list after Google sign-in, which only the public URL serves. With `--public-access anyone`, also anyone, as an anonymous caller. |
| Messengers | Accounts linked with a pairing code, and email senders on the `--allow-email` list. With `--public-access anyone`, also anyone in a direct chat, as an anonymous caller. Only verified senders count: for email, mail that passes DMARC, or DKIM of the sender's exact domain, at the trusted server. Email proves only the sender's domain. |
| The agent's socket | Only the agent, while it answers a message. A call there acts for the user who sent that message, and reaches only the tasks its conversation started. |

Only the host agent and its owners can call a webhook node, unless it is advertised with `pair invite --advertised`. Then other agents on the private network can call it through the host, which checks them like any other caller. Agents share only the names of advertised nodes, and calls always go to the host found by name on the private network. The card of a node that isn't advertised goes only to callers who may call the node.

A call that comes through another agent gets at most the authenticated role, even from your own agents, so someone who can steer one of your agents can't act as you on the others. The calling server keeps the user a call is for, and relays the callee's approvals to them.

What a role allows:

| | Owner | Authenticated | Anonymous |
| --- | --- | --- | --- |
| Talk to the agent and start tasks | yes | yes | yes, within a budget per listener |
| See tasks | all | their own | their own; on the public URL, the tasks whose task or conversation ID they have |
| Answer tool approvals | all | their own tasks; `--deny-tools` limits what they can run | no |
| Call other agents and use the schedules agent | yes | yes | no |
| Send a message into another user's conversation (A2A `contextId`) | yes | no | no |
| Continue another user's waiting task in a shared chat | yes | no | no |

Minting pairing codes, changing settings, adding contacts or messengers, and opening the inspector need access to the control socket in `state/`. No network role grants them.

## Calls to other agents and webhooks

The server calls contacts and wakes webhook nodes at the URLs you configure. A contact's card and a webhook's redirects can point the server elsewhere. When the URL you configured leads to a public address, the server connects only to public addresses for it. It refuses loopback, private, link-local and tailnet addresses, and it checks the address of each connection it opens. Contacts and webhooks that you configure on the tailnet or the LAN can reach those networks. A connection through a proxy set in the environment goes wherever the proxy sends it.

The server refuses redirects from https to http. A node sees only the HTTP status of a failed wake, and the server log has the details.

## Tokens and secrets

| Token | Grants | Stored | Revoke |
| --- | --- | --- | --- |
| Pairing code (`K7Q2-9XF4`) | One exchange for a token or a messenger link, within 15 minutes | Hashed in `server.db` | Expires; `a2a-serve pair rm <name>` or `pair set <name> --rotate` |
| Caller token (`a2p_…`) | Calls to this agent with the pairing's current role | Hashed in `server.db`; with the caller in `~/.config/a2a-serve/credentials.json` (mode 0600) | `a2a-serve pair rm <name>`, or `pair set <name> --rotate` for a new code. The caller forgets it with `a2a-serve pair rm <agent-url>` |
| Token of the `local` pairing | Calls to the local URL as the owner | Like a caller token, for the user running the server, while the server runs | `a2a-serve pair rm local`, which also unlinks your messenger accounts; the next start issues a new token |
| Messenger link | Messages from that account with the pairing's current role | `server.db` | `a2a-serve pair rm <name>` |
| Webhook node token (`a2rn.…`) | The node's mailbox, and calls to the agent as an authenticated caller | Hashed in `server.db`; with the node, in its credentials or `A2A_NODE_TOKEN` | `a2a-serve pair rm <node>` or `pair set <node> --rotate` |
| Google sign-in token (`a2p_…`) | Calls to this agent as an authenticated caller, while the email stays on the allowlist | Hashed in `server.db`; with the caller | `a2a-serve pair rm --email <address>`, or `a2a-serve config unset allow-email <entry>`, which deletes them within an hour |
| Wake signing key | Proves that wakes come from this agent. Derived from the node token, so the node needs no other secret | `server.db` | `a2a-serve pair set <node> --rotate` |
| Contact token | Calls to another agent, made by this server | Plain in `server.db` | `a2a-serve contact rm <name>`, and revoke it on the other agent |
| Agent key (Docker Desktop) | The agent's socket, for the conversations of one container | Generated when the container starts, in its home | Restart |
| Inspector link and session | The inspector, as the owner | In memory; the link works once for 5 minutes, the session for 12 hours | Restart |
| Relay identity | Membership of the Agent Mesh relay | `state/agentmesh` | Remove it on the relay, or delete the directory and join again |
| Relay invite (`mesh-bt-…`) | Joins one agent to the relay, which then keeps renewing its membership | With the agent, until it joins once; it expires after a day, or after `--expires` | Unused: let it expire. Used: `a2a-serve relay remove <agent>` on the relay |
| Relay admin token | The relay's control API: invite, list and remove agents | The relay's `state/relay/secrets` (mode 0600), or the container's environment | Stop the relay, then start it with a new one: `a2a-serve relay run --admin-token <value>` |
| `--secret` values, bot tokens, email password, Tailscale key, relay token, Google client secret, backup key | What the service behind them grants | `state/secrets` (mode 0600) | `a2a-serve config unset <setting>` or `a2a-serve channel rm <kind>`, then rotate at the service |

`state/` is mode 0700 and `state/secrets` 0600. `a2a-serve doctor` reports looser modes, and `doctor --fix` tightens them. Backups contain `state/secrets` and are always encrypted with the backup key.

## What a project can make your machine run

Starting an agent runs what its files say. Read the ones you didn't write before you start an agent from a cloned repository, as you would a Makefile or a CI config.

- **`.a2a/config.yaml`** decides:
  - `sandbox: local`: the agent runs with no isolation;
  - `command`: the agent's command line, run in the sandbox, or on the host with `sandbox: local`;
  - `image`: the container image;
  - `mounts` and `workspace`: the host directories the agent can read and write;
  - `env` and `secrets`: the variables the agent gets;
  - `backup`: where snapshots of the agent's secrets are uploaded;
  - webhook nodes: the URLs the server POSTs to.
- **`.mcp.json`** decides which MCP servers the agent starts in the sandbox and which URLs it calls. `${VAR}` in it can use only the agent's `--secret` and `--env` values, which the sandbox sees anyway. In the Docker sandbox, the agent can't read or edit the file. With `sandbox: local`, the agent can edit it, and Claude Code may start its servers on its own, including the ones `--disable-mcp` turns off.
- **Skills** in `.claude/skills`, `.opencode/skills` and `.agents/skills`, and instruction files such as `AGENTS.md` and `CLAUDE.md`, are instructions for the agent. Commands they suggest still wait for approval.

`a2a-serve recipe` shows what a [recipe](../guide/recipes.md) makes the agent run before it creates the agent, and marks risky entries with `!`: the local sandbox, read-write mounts, approvals of tool kinds that change things, and agent configuration files such as `.claude/settings.json`, which can change what the agent runs and approves. A recipe can't set where the agent is reachable, who may call it, its messengers or its backups. It can't bring files into `.a2a`, or symlinks that lead out of the recipe.
