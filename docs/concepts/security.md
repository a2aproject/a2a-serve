# Security

An a2a-serve agent can read your project, run commands and reach the network, and other people and agents can give it work. This page describes what the current version protects and what it doesn't yet. Report vulnerabilities as described in [SECURITY.md](../../SECURITY.md).

## The sandbox

By default the agent runs in Docker with:

- the workspace and the `--mount` directories, and nothing else from your disk. `.a2a/` appears as an empty read-only directory, so the agent can't read its own secrets, tokens, database or config;
- `state/home` as its home directory, where it keeps its sessions and config;
- your user ID (container root under rootless Docker, which maps to you), all capabilities dropped, `no-new-privileges`, and limits of 4 GB of memory, 2 CPUs and 512 processes;
- a socket to the server, through which it calls other agents. The server makes those calls with its own credentials, so contact tokens never enter the sandbox. On Docker Desktop the socket is a TCP port on the host's loopback, and the agent sends a key.

Every tool call that changes something waits for approval from the person who started the task, or an owner (see [Approvals](../guide.md#approvals)). The agent can't answer the approvals of agents it calls; the server relays them to the user.

What the sandbox doesn't protect yet:

- **API keys are readable inside it.** Values passed with `--secret` are environment variables of the agent, so a tricked agent can read them and send them anywhere ([#16](https://github.com/a2aproject/a2a-serve/issues/16)).
- **It can reach any host.** A tricked agent can send the files it can read anywhere ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **The workspace is writable** unless mounted `:ro`, including `.git`, `.mcp.json` and skills. A change the agent makes there applies to the next conversation, and to you when you run tools in the project.
- **Docker is the boundary.** Kernel and Docker escapes are out of scope for this project.

`--sandbox local` runs the agent on the host, as you, with no isolation. It can read everything you can, including `~/.config/a2a-serve/credentials.json` with your owner token. Use it only for agents and callers you trust completely.

## Who can call the agent

Every A2A request needs a known caller. These routes are public: the agent card, `/pair` (which redeems codes and limits failed attempts), `/skill.md`, `/download/…` and `/hitl/<token>`.

A web page open in your browser can't make requests that change state on the agent, because the server refuses cross-site requests and needs a JSON body ([browser requests](../reference/http-routes.md#browser-requests)).

| Network | Who gets in |
| --- | --- |
| localhost | Callers with a token. On start, the server saves an owner token named `local` for the user running it, which works only on the local URL. A loopback connection alone grants nothing, because tunnels also reach the server from loopback. |
| Tailscale | Your own devices, identified by Tailscale as your login, are owners. Other tailnet users get a role from `--identity login=role` or the `a2aproject.org/cap/a2a` app capability. Anyone else needs a token. |
| LAN | Callers with a token. The LAN has no identity, so being on it grants nothing. |
| Agent Mesh relay | Agents signed in to the relay as you are owners. Other agents get a role from `--identity`. |
| Public URL (Funnel, ngrok, relay) | Callers with a token, including people on the `--allow-email` list after Google sign-in. |
| Messengers | Accounts linked with a pairing code. Only verified senders can link: for email, mail that passes DMARC or aligned DKIM at the trusted server. |
| The agent's socket | Only the agent, while it answers a message. A call there acts for the user who sent that message, and reaches only the tasks its conversation started. |

Only the host agent and its owners can call a webhook node, unless it is advertised with `pair invite --advertised`. Then other agents on the private network can call it through the host, which checks them like any other caller. Agents share only the names of advertised nodes, and calls always go to the host found by name on the private network.

Your own agents on the tailnet can say which user a call is for (`X-A2A-Serve-On-Behalf-Of`). Other callers can't; the header is ignored from them.

What a role allows:

| | Owner | Member |
| --- | --- | --- |
| Talk to the agent and start tasks | yes | yes |
| See tasks | all | their own |
| Answer tool approvals | all | their own tasks; `--deny-tools` limits what they can run |
| Send a message into another user's conversation (A2A `contextId`) | yes | no |
| Continue another user's waiting task in a shared chat | yes | no |

Minting pairing codes, changing settings, adding contacts or messengers, and opening the inspector need access to the control socket in `state/`. No network role grants them.

## Tokens and secrets

| Token | Grants | Stored | Revoke |
| --- | --- | --- | --- |
| Pairing code (`K7Q2-9XF4`) | One exchange for a token or a messenger link, within 15 minutes | Hashed in `server.db` | Expires; `a2a-serve pair rm <name>` or `pair set <name> --rotate` |
| Caller token (`a2p_…`) | Calls to this agent with the pairing's role | Hashed in `server.db`; with the caller in `~/.config/a2a-serve/credentials.json` (mode 0600) | `a2a-serve pair rm <name>`, or `pair set <name> --rotate` for a new code. The caller forgets it with `a2a-serve pair rm <agent-url>` |
| Owner token `local` | Calls to the local URL as the owner | Like a caller token, for the user running the server | `a2a-serve pair rm local`; the next start issues a new one |
| Messenger link | Messages from that account with the pairing's role | `server.db` | `a2a-serve pair rm <name>` |
| Webhook node token (`a2rn.…`) | The node's mailbox, and calls to the agent as a member | Hashed in `server.db`; with the node, in its credentials or `A2A_NODE_TOKEN` | `a2a-serve pair rm <node>` or `pair set <node> --rotate` |
| Google sign-in token (`a2p_…`) | Calls to this agent as a member, while the email stays on the allowlist | Hashed in `server.db`; with the caller | `a2a-serve config unset allow-email <entry>`, or `a2a-serve pair rm google:<email>` |
| Webhook signing secret | Proves that wakes come from this agent | `server.db` | `a2a-serve pair set <node> --rotate` |
| Contact token | Calls to another agent, made by this server | Plain in `server.db` | `a2a-serve contact rm <name>`, and revoke it on the other agent |
| Agent key (Docker Desktop) | The agent's socket | Generated on start, in the agent's home | Restart |
| Inspector link and session | The inspector, as the owner | In memory; the link works once for 5 minutes, the session for 12 hours | Restart |
| Relay identity | Membership of the Agent Mesh relay | `state/agentmesh` | Remove it on the relay, or delete the directory and join again |
| `--secret` values, bot tokens, email password, Tailscale and ngrok keys, relay token, Google client secret, backup key | What the service behind them grants | `state/secrets` (mode 0600) | `a2a-serve config unset <setting>` or `a2a-serve channel rm <kind>`, then rotate at the service |

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
- **`.mcp.json`** decides which MCP servers the agent starts in the sandbox and which URLs it calls. `${VAR}` in it can use only the agent's `--secret` and `--env` values, which the sandbox sees anyway. The agent can edit the file, and the change applies to the next conversation.
- **Skills** in `.claude/skills`, `.opencode/skills` and `.agents/skills`, and instruction files such as `AGENTS.md` and `CLAUDE.md`, are instructions for the agent. Commands they suggest still wait for approval.
