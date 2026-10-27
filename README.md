# A2A Serve

<div align="center" width="800">
   <img src="https://raw.githubusercontent.com/a2aproject/A2A/refs/heads/main/docs/assets/a2a_logo/color/SVG/a2a_color.svg" width="600" alt="Agent2Agent Protocol Logo"/>
   <h3>
       Serve a coding agent over A2A from its project directory.
   </h3>
</div>

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/a2aproject/a2a-serve" alt="License: Apache-2.0"></a>
</p>

> [!WARNING]
> Alpha. Commands, flags and stored state may change.

**A2A Serve** (`a2a-serve`) runs a coding agent (opencode, Claude Code, or any ACP agent) as an [A2A (Agent2Agent)](https://a2a-protocol.org/latest/) server. It is a command plugin of the [A2A CLI](https://github.com/a2aproject/a2a-cli): with `a2a-serve` on your `PATH`, it runs as `a2a serve`. It also works on its own, as `a2a-serve`.

## Why A2A Serve

A coding agent runs in a terminal on one machine, and only the person at that terminal can give it work. A2A Serve puts an A2A server in front of the agent, so other people, other agents and scripts can send it tasks with the `a2a` CLI.

1. **Run agents safely** — the agent runs in a Docker sandbox, and tool calls it asks about wait for an approver.
1. **Reach them from anywhere** — over Tailscale, the LAN or ngrok, and from Telegram, Discord and email.
1. **Connect agents** — pair callers with one-time codes, and let agents call each other and webhook nodes through the server.
1. **Test locally** — echo, proxy and exec servers for trying A2A clients without a real agent.

<div align="center">
  <img src="./docs/graphs/readme-overview.png" alt="Callers reach a2a serve, which runs the coding agent in a Docker sandbox; the agent calls other agents through the server" width="720">
</div>

## Installation

Install `a2a-serve` and the `a2a` CLI: callers use `a2a` to send tasks, and harnesses use it to call other agents. Until an `a2a` release has `--unix-socket` and credential helpers, install this version:

```bash
go install github.com/a2aproject/a2a-serve@latest
go install github.com/a2aproject/a2a-cli@v0.3.2-0.20261027071959-4abaf842d69f
# go install names the binary `a2a-cli`; rename it to `a2a`
mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"
a2a-serve doctor --fix   # enable the plugin and the credential helper, check Docker
```

<details>
<summary>Without <code>doctor --fix</code></summary>

```bash
a2a plugin set-enabled true   # let `a2a` run a2a-serve as `a2a serve`
mkdir -p ~/.config/a2a-cli
grep -q '^credential-helper:' ~/.config/a2a-cli/config.yaml 2>/dev/null ||
  echo 'credential-helper: a2a-serve credential' >> ~/.config/a2a-cli/config.yaml
```

</details>

The server mounts the `a2a` from its `PATH` into Linux sandboxes and builds harness images with the same version.

Run `a2a serve doctor` whenever something looks wrong: it checks the `a2a` CLI and its config, Docker, and the agent of the current directory, and prints the command which fixes each failure. If `a2a serve` fails with `unknown command "serve" for "a2a"`, command plugins are disabled or `a2a-serve` is not on `PATH`; run `a2a-serve doctor --fix`.

## Usage

From the agent's project directory:

```bash
cd ~/src/app
a2a serve opencode --name coder --secret OPENROUTER_API_KEY --private tailscale --telegram env:CODER_BOT_TOKEN
a2a send -a "$(a2a serve url --local)" "What does this project do?"   # from this machine: no pairing
a2a serve pair invite anna   # a one-time code for anyone else; they run: a2a serve tokens add <agent-url> <code>
```

The first run saves the flags to `.a2a/config.yaml`; later runs are `a2a serve`. Change settings later with the same flags: `a2a serve config set --sandbox local`.

| Command | Description |
|---|---|
| `serve <harness>` | Create the agent with the flags and start it; in an existing agent, apply the flags like `config set`, then start it. `--detach`, `--service`. |
| `serve` | Start the agent of the nearest `.a2a/` directory (or `--dir`). Asks a few questions on a first run in a terminal. |
| `serve config [show]` | The effective settings, secrets as names with their state, and changes waiting for a restart. `--defaults`, `--as-command`, `-o json\|yaml`. |
| `serve config set <flags>` / `unset <setting> [<entry>…]` / `edit` | Change settings with the flags of `serve <harness>`, validated and printed as a diff. A running agent applies channels, the description and approvers at once. `--dry-run`, `--restart`. |
| `serve status` / `logs [-f]` / `stop [--uninstall]` / `restart` | Manage the running agent; `stop --uninstall` also removes its service, `restart` starts it again in the same mode. |
| `serve url [--local\|--private\|--public]` | The agent's URL: public, then private, then local. `a2a send -a "$(a2a serve url)" …` |
| `serve pair invite <who> [--role owner]` | Create a one-time code for a caller or a messenger account. The user running the server needs none: the server saves an owner token for its local URL. |
| `serve pair invite <id> --webhook <url>` | Host a webhook node and create its code; without `--webhook`, a new code for an existing node. |
| `serve pair list` / `pair rm <name>` | List callers and nodes, or revoke a caller / remove a node. |
| `serve tokens add <agent-url> <code>` | On the caller's side: exchange a code for a token, which `a2a` attaches to requests for that agent through the `serve credential` helper. `tokens list` / `tokens rm <agent-url>` manage the saved tokens. |
| `serve doctor [--fix] [--online]` | Check the `a2a` CLI, Docker and the agent; `--fix` enables the plugin and the credential helper and makes state files private. `-o json`. |
| `serve credential get` | The credential helper for the `a2a` CLI. |
| `serve completion <shell>` | A completion script for `a2a-serve`: commands, harnesses, channel kinds, enum flags, and the contacts and pairings of a running agent. |
| `serve peers` | Agents found on the private network. |
| `serve contact add\|list\|rm` | Agents this agent may call with the `a2a` CLI, through the server. |
| `serve channel add\|rm <telegram\|discord\|email>` / `channel list` | Messengers; `list` shows whether they run. |
| `serve relay reply\|task\|inbox` | Commands a webhook node runs. |
| `serve echo` / `proxy <agent-url>` / `script '<command>'` | A local test server: echo the message, forward to another agent, or run a shell command per message (stdin is the message, stdout the reply; `--stream`, `--chunk <delim>`). |

See the **[agent server guide](./docs/agent-server.md)** and its **[architecture](./docs/agent-server-architecture.md)** for sandboxing, approvals, networks, messengers, agent-to-agent calls and webhook nodes.

## Cookbook

Learn A2A Serve by example. Each lesson in the **[cookbook](./examples/)** is a folder you can run on its own:

1. **[Serve a coding agent](./examples/01-agent-server/)** — turn opencode or Claude Code into an A2A agent, pair with it, send it work, approve a tool call, and let it call a second agent.
2. **[Tailscale, Telegram, and a Grok bot](./examples/02-tailscale-telegram-grok/)** — reach the agent from your tailnet, a public Funnel URL and a Telegram chat, host a Grok bot as a webhook node, and run it as a service.

## What It Needs from the A2A CLI

- Command plugins: `a2a-<name>` on `PATH` runs as `a2a <name>`.
- `--unix-socket` (`A2ACLI_UNIX_SOCKET`): harnesses in a Linux sandbox reach the server through a mounted Unix socket.
- Credential helpers (`credential-helper`, the [Bazel credential helper protocol](https://github.com/EngFlow/credential-helper-spec)): `a2a-serve credential` returns the token `a2a serve tokens add` saved in `~/.config/a2a-serve/credentials.json` for the longest matching agent URL, or `$A2A_RELAY_TOKEN` for its agent. Enable it once in `~/.config/a2a-cli/config.yaml` with `credential-helper: a2a-serve credential`. On Docker Desktop the harness gets its key from a helper script scoped to the bridge host.
- `a2a skill` and `a2a version -o json`, which the server serves at `/skill.md` and uses to tag harness images.

## How to Contribute & Provide Feedback

Open an issue or pull request to share suggestions, questions, or edge cases. See the **[Contributing guide](CONTRIBUTING.md)**.

## License

A2A Serve is open-source and licensed under the [**Apache License 2.0**](LICENSE).
