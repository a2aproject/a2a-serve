# a2a serve

> [!WARNING]
> Alpha. Commands, flags and stored state may change.

`a2a-serve` runs a coding agent (opencode, Claude Code, or any ACP agent) as an
[A2A](https://a2a-protocol.org/latest/) server from its project directory. The
agent is sandboxed in Docker and can be reached over Tailscale, the LAN, ngrok,
Telegram, Discord and email. It also runs local echo, proxy and exec test servers.

It is a command plugin of the [A2A CLI](https://github.com/a2aproject/a2a-cli):
with `a2a-serve` on your `PATH`, it runs as `a2a serve`. It also works on its
own, as `a2a-serve`.

## Installation

```bash
go install github.com/a2aproject/a2a-serve@latest
a2a plugin set-enabled true   # let `a2a` run a2a-serve as `a2a serve`
mkdir -p ~/.config/a2a-cli && echo 'credential-helper: a2a-serve credential' >> ~/.config/a2a-cli/config.yaml
```

You also need the `a2a` CLI: callers use it to send tasks, and harnesses use it
to call other agents. Until a release has `--unix-socket` and credential
helpers, install this version:

```bash
go install github.com/a2aproject/a2a-cli@v0.3.2-0.20261027071959-4abaf842d69f
mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"
```

The server mounts the `a2a` from its `PATH` into Linux sandboxes and builds
harness images with the same version.

## Usage

From the agent's project directory:

```bash
cd ~/src/app
a2a serve opencode --name coder --secret OPENROUTER_API_KEY --private tailscale --telegram env:CODER_BOT_TOKEN
a2a send -a http://127.0.0.1:<port> "What does this project do?"   # from this machine: no pairing
a2a serve pair invite anna   # a one-time code for anyone else; they run: a2a serve pair add <agent-url> <code>
```

The first run saves the flags to `.a2a/config.yaml`; later runs are `a2a serve`.

| Command | Description |
|---|---|
| `serve <harness>` | Save the flags to `.a2a/config.yaml` and start the agent. `--detach`, `--service`. |
| `serve` | Start the agent of the nearest `.a2a/` directory (or `--dir`). Asks a few questions on a first run in a terminal. |
| `serve status` / `logs [-f]` / `stop [--uninstall]` | Manage the running agent; `stop --uninstall` also removes its service. |
| `serve pair invite <who> [--role owner]` | Create a one-time code for a caller or a messenger account. The user running the server needs none: the server saves an owner token for its local URL. |
| `serve pair invite <id> --webhook <url>` | Host a webhook node and create its code; without `--webhook`, a new code for an existing node. |
| `serve pair list` / `pair rm <name>` | List callers and nodes, or revoke a caller / remove a node. |
| `serve pair add <agent-url> <code>` | On the caller's side: exchange a code for a token, which `a2a` attaches to requests for that agent through the `serve credential` helper. `pair tokens` / `pair forget <agent-url>` manage the saved tokens. |
| `serve credential get` | The credential helper for the `a2a` CLI. |
| `serve peers` | Agents found on the private network. |
| `serve contact add\|list\|rm` | Agents this agent may call with the `a2a` CLI, through the server. |
| `serve channel add\|rm <telegram\|discord\|email>` | Messengers. |
| `serve relay reply\|task\|inbox` | Commands a webhook node runs. |
| `serve --echo` / `--proxy <agent-url>` / `--exec <cmd>` | A local test server: echo the message, forward to another agent, or run a command per message. |

See the **[agent server guide](./docs/agent-server.md)** and its
**[architecture](./docs/agent-server-architecture.md)**, and the
**[examples](./examples/)**.

## What it needs from the a2a CLI

- Command plugins: `a2a-<name>` on `PATH` runs as `a2a <name>`.
- `--unix-socket` (`A2ACLI_UNIX_SOCKET`): harnesses in a Linux sandbox reach the server through a mounted Unix socket.
- Credential helpers (`credential-helper`, the [Bazel credential helper protocol](https://github.com/EngFlow/credential-helper-spec)): `a2a-serve credential` returns the token `a2a serve pair add` saved in `~/.config/a2a-serve/credentials.json` for the longest matching agent URL, or `$A2A_RELAY_TOKEN` for its agent. Enable it once in `~/.config/a2a-cli/config.yaml` with `credential-helper: a2a-serve credential`. On Docker Desktop the harness gets its key from a helper script scoped to the bridge host.
- `a2a skill` and `a2a version -o json`, which the server serves at `/skill.md` and uses to tag harness images.

## License

[Apache 2.0](./LICENSE)
