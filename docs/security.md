# Security

> [!WARNING]
> A2A Serve is pre-1.0. This page describes what the current version
> protects and what it doesn't yet. Report vulnerabilities as described in
> [SECURITY.md](../SECURITY.md).

An agent server runs a coding agent that can read your project, run commands
and reach the network, and lets other people and agents give it work. This
page covers four questions: what the sandbox protects, who can call the agent,
what each token grants, and what files in a project can make your machine
run.

## The sandbox

By default the harness runs in Docker with:

- the workspace and the `--mount` directories, and nothing else of your
  disk. `.a2a/` shows up as an empty read-only directory, so the harness can't
  read the agent's secrets, tokens, database or config;
- `state/home` as its home directory (`/home/agent`), where it keeps its
  sessions and config;
- your user ID (container root under rootless Docker, which maps to you), all
  capabilities dropped, `no-new-privileges`, and limits of 4 GB of memory,
  2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`);
- a Unix socket, the harness listener, through which it calls other agents.
  The server makes those calls with its own credentials, so contact tokens
  never enter the sandbox. On Docker Desktop the listener is TCP on the host's
  loopback, and the harness sends a key that a helper script in its home
  returns.

Every tool call that changes something asks first, and whoever started the
task, or an owner, answers (see [Approvals](agent-server.md#approvals)). The harness
can't answer approvals of the agents it calls; the server relays them to the
user.

What the sandbox doesn't protect yet:

- **API keys are accessible inside it.** Values passed with `--secret` are
  environment variables of the harness, so a tricked agent can read and send
  them anywhere ([#16](https://github.com/a2aproject/a2a-serve/issues/16)).
- **It can reach any host.** The container has normal network access, so a
  tricked agent can send the files it can read anywhere
  ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **Everything in the workspace is writable** unless it is mounted `:ro`,
  including `.git`, the project's `.mcp.json` and its skills. A change the
  agent makes there applies to the next conversation, and to you when you run
  tools in the project.
- **Docker itself is the boundary.** Kernel or Docker escapes aren't in scope
  of this project.

`--sandbox local` runs the harness on the host, as you, with no isolation. It
can read everything you can, including `~/.config/a2a-serve/credentials.json`
with the owner token of this machine. Use it only for agents and callers you
trust completely.

## Who can call the agent

Every A2A request needs a principal. The agent card, `/pair` (which redeems
codes, with a limit on failed attempts), `/skill.md`, the binaries at
`/download/…` and `/hitl/<token>` pages are public.

| Network | Who gets in |
| --- | --- |
| localhost | Callers with a token. The server saves an owner token named `local` for its local URL in the credentials of the user running it. Loopback connections aren't trusted by themselves, because tunnels reach the server from loopback too. |
| Tailscale (private) | Your own machines on the tailnet, identified by Tailscale `WhoIs` as the same login, are the owner. Other tailnet users get a role only from `--identity login=role` or the `a2aproject.org/cap/a2a` app capability. Anyone else needs a token. |
| LAN (private) | Callers with a token. The LAN has no identity, so being on it grants nothing. |
| Funnel, ngrok or a relay's public URL | Callers with a token, including people on the `--allow-email` list once they sign in with Google. |
| Agent Mesh relay (private) | Agents signed in to the relay as the same person are the owner. Other agents on the relay get a role only like other private-network callers (`--identity`). The private URL works only from other a2a-serve agents on the relay. |
| Messengers | Accounts linked with a pairing code. Only verified senders can link: for email, messages that pass DMARC or aligned DKIM at the trusted server. |
| Harness listener | Only the harness, which reaches the socket. A call there acts for the user of the conversation it came from. |

A webhook node is called only by its host and the host's owner, unless it is
advertised with `pair invite --advertised`: then members of the host, such as
other people's agents on the private network, can call it too. Gossip between
agents only spreads the names of advertised nodes; a call to one goes to the
host found by name on the private network.

An agent of the same owner on the tailnet can say whom it calls for
(`X-A2A-Serve-On-Behalf-Of`); the header is ignored from everyone else.

What a role allows:

| | Owner | Member |
| --- | --- | --- |
| Talk to the agent and start tasks | yes | yes |
| See tasks | all | their own |
| Answer tool approvals | all | of their own tasks; `--deny-tools` limits what they can run |
| Continue another user's waiting task in a shared chat | yes | no |

Only someone with access to the agent directory, through the control socket
in `state/`, can mint pairing codes, change settings, add contacts or
channels, and open the web inspector. No network role grants that.

## Tokens and secrets

| Token | Grants | Stored | Revoke |
| --- | --- | --- | --- |
| Pairing code (`K7Q2-9XF4`) | One exchange for a token or a messenger link, within 15 minutes | Hashed in `server.db` | Expires; `a2a-serve pair rm <name>` or `pair set <name> --rotate` |
| Caller token (`a2p_…`) | Calls to this agent as its principal, owner or member | Hashed in `server.db`; in the caller's `~/.config/a2a-serve/credentials.json` (mode 0600) | `a2a-serve pair rm <name>`, or `pair set <name> --rotate`, which keeps the pairing and prints a new code; the caller drops it with `a2a-serve pair rm <agent-url>` |
| Owner token `local` | Calls to the local URL as the owner | Same as a caller token, for the user running the server | `a2a-serve pair rm local`; the next start issues a new one |
| Messenger link | Messages from that account as its principal | `server.db` | `a2a-serve pair rm <name>` |
| Webhook node token (`a2rn.…`) | The node's mailbox API, and calls to the agent as a member | Hashed in `server.db`; with the node, in its credentials or `A2A_NODE_TOKEN` | `a2a-serve pair rm <node>`, `pair set <node> --rotate`, or a new code for the node |
| Google sign-in token (`a2p_…`, principal `google:<email>`) | Calls to this agent as a member, while the email stays on the allowlist | Hashed in `server.db`; with the caller | `a2a-serve config unset allow-email <entry>`, which takes effect at the next request, or `a2a-serve pair rm google:<email>` |
| Webhook signing secret | Proves wakes come from this agent | `server.db` | `a2a-serve pair set <node> --rotate` |
| Contact token | Calls to another agent, made by this server | Plain in this agent's `server.db` | `a2a-serve contact rm <name>`, and revoked on the other agent |
| Harness key (Docker Desktop) | The harness listener | Generated on start, in the harness home | Restart |
| Inspector login link and session | The web inspector, as the owner | In memory; the link works once for 5 minutes, the session for 12 hours | Restart |
| Relay identity | Membership of the Agent Mesh relay | `state/agentmesh` | Remove it on the relay, or delete the directory and join again |
| `--secret`, bot tokens, email password, Tailscale and ngrok keys, relay token, Google client secret, backup key | What the service behind them grants | `state/secrets` (mode 0600) | `a2a-serve config unset <setting>` or `a2a-serve channel rm <kind>`, then rotate at the service |

`state/` is mode 0700 and `state/secrets` 0600; `a2a-serve doctor` reports
looser modes and `doctor --fix` tightens them. Backups contain `state/secrets`
and are always encrypted with the backup key.

## What a project can make your machine run

Starting an agent runs what its files say. Read the ones you didn't write
before you start an agent from a cloned repository, as you would a Makefile
or a CI config.

- **`.a2a/config.yaml`** decides, on the host:
  - `sandbox: local` runs the harness with no isolation;
  - `command` replaces the harness command line, run inside the sandbox, or
    on the host with `sandbox: local`;
  - `image` picks the container image;
  - `mounts` and `workspace` pick the host directories the harness can read
    and write;
  - `env` and `secrets` pick the variables the harness gets;
  - `backup` picks where snapshots of the agent's secrets are uploaded;
  - webhook nodes make the server POST to their URLs.
- **`.mcp.json`** in the workspace decides which MCP servers the harness
  starts, inside the sandbox, and which URLs it calls. `${VAR}` in it can take
  only the agent's `--secret` and `--env` values, which the sandbox sees
  anyway. The harness can edit the file, and the change applies to the next
  conversation.
- **Skills** in `.claude/skills`, `.opencode/skills` and `.agents/skills` are
  instructions for the harness, and may tell it to run commands, which still
  ask for approval.
- **Instruction files** such as `AGENTS.md` and `CLAUDE.md` in the project
  are read by the harness the same way.
