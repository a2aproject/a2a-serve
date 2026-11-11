# Security

This page describes what protects you when an agent reads your project, runs commands and takes work from other people and agents, and what isn't protected yet. Report vulnerabilities as described in [SECURITY.md](../../SECURITY.md).

## The sandbox

By default the agent runs in a Docker container as your user, with no capabilities and with resource limits. It sees the workspace and the `--mount` directories, and nothing else from your disk. Its own `.a2a/` directory, the `.a2a/` directories of other agents in the mounts, `.mcp.json` and the agent's settings files in the workspace appear empty, so the agent can't read its secrets or change its own permissions. [Sandbox](../guide/sandbox.md) shows what the agent sees.

The container has no network of its own. Every connection goes through the server, which allows the public internet and blocks private destinations, following the [agents.net](https://github.com/aojea/agents.net) specification. The agent calls other agents through the server too, so contact tokens never enter the sandbox.

Each [member](../guide/identities.md#roles) gets a container and home of their own, and [guests](../guide/identities.md#guests) share one more. What each container gets of the workspace and the secrets follows the [sandbox defaults](../guide/sandbox.md#what-each-callers-container-gets).

Tool calls that change something wait for approval from the person who started the task, or an owner ([Approvals](../guide/approvals.md)). Approval rules trust what the agent reports about a call. Approvals keep you informed and able to step in. The sandbox and scoped credentials are what protect you.

What the sandbox doesn't protect yet:

- **`--secret` values are readable inside it.** Every container that gets a secret has it as an environment variable, so a tricked agent, or a caller who approves their own commands, can read it and send it anywhere. [`--inject-model-key` and `--inject-header`](../guide/secrets.md#keep-keys-out-of-the-sandbox) keep a key out of the container, though the agent can still spend it. Give guests and members you trust less a [separate key](../guide/sandbox.md#give-guests-a-key-of-their-own).
- **It can reach any public host.** A tricked agent can send the files it can read anywhere on the internet. `--egress allowlist` limits it to the hosts you list.
- **A host you allow is reachable from every container**, including the containers of members and guests.
- **Without the network boundary, it reaches the tailnet and the LAN as you.** This happens with `--egress unrestricted`, `--sandbox local`, and the setups that [Where it applies](../guide/sandbox.md#where-it-applies) lists. Your other agents and tailnet services then see your device and grant your rights, whoever started the task.
- **A shared workspace is writable**, including `.git` and skills. A change the agent makes there applies to the next conversation, and to you when you run tools in the project.
- **`docker exec` into a sandbox runs as root** with the capabilities the container starts with. Pass `--user` with your user ID.
- **An agent created inside the mounts of a running agent** is visible to that agent's running containers until `a2a-serve restart`.
- **Docker is the boundary.** Kernel and Docker escapes are out of scope.

`--sandbox local` runs the agent on the host, as you, with no isolation. Secrets and storage rules don't confine it. It can read everything you can, including your owner token, the `.a2a/` directories of your other agents and the conversations of every user. Use it only when you trust the agent and everyone who can call it completely.

## Who can call the agent

Every A2A request needs a known caller. The [Access Model](access-model.md) describes who that is on each network and what each role may do. The public routes, such as the agent card and code redemption, are bounded per caller ([request limits](../reference/http-routes.md#request-limits)). A web page in your browser can't change state on the agent ([browser requests](../reference/http-routes.md#browser-requests)).

Trust follows the network:

- **The private network** is trusted. Its members can call the agent, and your own untagged tailnet devices act as you. A compromised device of yours is out of scope.
- **Email** proves only the sender's domain. Anyone who can send verified mail for a domain counts as any of its addresses, including an address you linked to an owner pairing. Your mail server must remove `Authentication-Results` headers that carry its own name from incoming mail, as mail servers do.
- **A call through another agent** gets at most the member role, even from your own agents.

Creating pairing codes, changing settings and opening the inspector need access to the agent directory. No network role grants them.

## Calls to other agents and webhooks

A contact or webhook on a public URL can't point the server into your private network: the server refuses redirects and addresses that lead from a public URL to loopback, private, link-local or tailnet addresses, and redirects from https to http. Contacts and webhooks that you configure on the tailnet or the LAN can reach those networks.

The description and skills from the cards of other agents reach your agent's instructions. The server shortens them and labels them as the other agent's description of itself.

A wake signature stays valid for a few minutes, so a node that must not act twice on the same wake should skip wake IDs it has already seen ([signatures](../reference/http-routes.md#signatures-and-headers)).

## Tokens and secrets

| Token | Stored | Revoke |
| --- | --- | --- |
| Pairing code | Hashed in `server.db` | Expires; `a2a-serve pair rm <name>` |
| Caller token (`a2p_…`) | Hashed in `server.db`; with the caller in `~/.config/a2a-serve/credentials.json` | `a2a-serve pair rm <name>` or `a2a-serve pair set <name> --rotate` |
| Local token of the owner | Like a caller token, while the server runs | `a2a-serve pair rm local`; the next start issues a new one |
| Messenger link | `server.db` | `a2a-serve pair rm <name>` |
| Webhook node token (`a2rn.…`) | Hashed in `server.db`; with the node | `a2a-serve pair rm <node>` or `a2a-serve pair set <node> --rotate` |
| Google sign-in token | Hashed in `server.db`; with the caller | Expires when unused; `a2a-serve pair rm --email <address>` |
| Contact token | Plain in `server.db` | `a2a-serve contact rm <name>`, and revoke it on the other agent |
| `--secret` values, bot tokens and other service keys | `state/secrets` | `a2a-serve config unset <setting>`, then rotate at the service |

`state/` is readable only by you. `a2a-serve doctor --fix` tightens loose file modes. Backups contain `state/secrets` and are always encrypted. [Relays](../guide/relays.md) covers the relay's tokens.

## What a project can make your machine run

Starting an agent runs what its files say. Read the files you didn't write before you start an agent from a cloned repository, as you would a Makefile or a CI config.

- **`.a2a/config.yaml`** decides the agent's command, image, mounts, secrets, network, who may call it, which tool calls run without asking, where backups go, and which speech service gets voice notes. `sandbox: local` turns off isolation, and `allowBroadMounts` can expose your home directory.
- **`.a2a/state/`** holds secrets, pairings and the network identity. Delete a `state/` that came with a repository before you start the agent.
- **`.a2a/recipe.lock`** names the repository that `a2a-serve recipe` fetches. Check that you trust it.
- **`.mcp.json`** decides which MCP servers the agent starts and which URLs it calls.
- **Agent settings files**, such as `opencode.json` and `.claude/settings.json`, change what the agent runs and approves.
- **Skills and instruction files**, such as `AGENTS.md`, instruct the agent. Commands they suggest still wait for approval.

In the Docker sandbox, the agent can't read or edit `.mcp.json` or its settings files, and Claude Code runs no hooks. With `sandbox: local`, the agent can edit them, and the hooks and `allowed-tools` of skills run without asking.

`a2a-serve recipe` shows what a [recipe](../guide/recipes.md) makes the agent run before it creates the agent, and marks risky entries with `!`. A recipe can't set where the agent is reachable, who may call it, its messengers or its backups.
