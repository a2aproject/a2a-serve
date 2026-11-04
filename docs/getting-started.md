# Getting started

## Install

`a2a-serve` works on its own, and is also a command plugin of the [A2A CLI](https://github.com/a2aproject/a2a-cli): with it on your `PATH`, every `a2a-serve` command also runs as `a2a serve`. The docs write `a2a-serve`. Callers use `a2a` to send tasks, and harnesses use it to call other agents. One step installs both:

```bash
brew install a2aproject/a2a-cli/a2a-serve    # macOS and Linux; brings the a2a formula too
a2a-serve doctor --fix                       # enable the plugin and the credential helper, check Docker
```

Without Homebrew, the install script downloads both release binaries to `~/.local/bin`, checks their checksums, runs `a2a-serve doctor --fix`, and offers to set up an agent in the current directory:

```bash
curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
```

`INSTALL_DIR`, `A2A_SERVE_VERSION` and `A2A_CLI_VERSION` change where and what it installs. Windows builds are attached to each release but untested.

From source, until an `a2a` release has `--unix-socket` and credential helpers:

```bash
go install github.com/a2aproject/a2a-serve@latest
go install github.com/a2aproject/a2a-cli@v0.3.2-0.20261030153310-f90e4eb628cd
# go install names the binary `a2a-cli`; rename it to `a2a`
mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"
a2a-serve doctor --fix
```

Without `doctor --fix`:

```bash
a2a plugin set-enabled true   # let `a2a` run a2a-serve as `a2a serve`
mkdir -p ~/.config/a2a-cli
grep -q '^credential-helper:' ~/.config/a2a-cli/config.yaml 2>/dev/null ||
  printf 'credential-helper:\n  - command: [a2a-serve, credential]\n' >> ~/.config/a2a-cli/config.yaml
```

Harnesses in Docker call other agents with the `a2a` of their image, which carries the version this `a2a-serve` needs; only `--sandbox local` uses the `a2a` on your `PATH`.

Run `a2a-serve doctor` whenever something looks wrong: it checks the `a2a` CLI and its config, Docker, and the agent of the current directory, and prints the command which fixes each failure. If `a2a serve` fails with `unknown command "serve" for "a2a"`, command plugins are disabled or `a2a-serve` is not on `PATH`; run `a2a-serve doctor --fix`.

## Start an agent

```bash
cd ~/src/app
a2a-serve start opencode --name coder --secret OPENROUTER_API_KEY
```

The first run saves the flags to `.a2a/config.yaml` and starts the agent in the foreground. Later runs, from the project or a subdirectory, are just `a2a-serve start`. Run `a2a-serve start` without arguments in a new directory and it asks a few questions instead. Change settings later with the same flags, through `a2a-serve config set` (see [Changing settings](guide.md#changing-settings)).

On start, the server saves an owner token for its local URL in your `~/.config/a2a-serve/credentials.json` and prints how to call it. The `a2a` CLI attaches it through a2a-serve's credential helper. From another terminal on the same machine:

```bash
a2a send -a "$(a2a-serve status --url=local)" "What does this project do?"
```

Anyone else needs an invitation:

```bash
a2a-serve pair invite anna                   # in ~/src/app: prints a one-time code
a2a-serve pair join <agent-url> <code>       # on anna's side: saves a token for that URL
```

`a2a send`, `a2a task …` and `a2a card get` attach the saved token automatically.

## Harnesses

| Harness | Image | Version |
|---|---|---|
| `opencode` | `ghcr.io/a2aproject/harness-opencode` | `opencode-ai`, the latest release when the image was published |
| `claude` | `ghcr.io/a2aproject/harness-claude` | `@zed-industries/claude-agent-acp` and `@anthropic-ai/claude-code`, the latest releases when the image was published |
| `acp --command …` | any, with `--image` | any agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com) |

`--command` also replaces a profile's command, such as `a2a-serve start opencode --command "cloudcode acp"`. Both images are Debian with Node.js 22, `git`, `ripgrep`, `python3`, and the `a2a` CLI at the version this `a2a-serve` needs, which the image tag names (`:a2a-<version>`).

## Platforms

| Platform | Status |
|---|---|
| Linux (amd64, arm64) with Docker, rootful or rootless | Supported. The harness reaches the server through a Unix socket. `--service` installs a systemd user unit. |
| macOS (Apple silicon, Intel) with Docker Desktop | Supported. Docker Desktop can't mount the socket, so the harness reaches the server over TCP on the host's loopback with a key. `--service` installs a launchd agent. |
| Linux and macOS without Docker | `--sandbox local` only: the harness runs on the host with no isolation. |
| Windows | Builds, but untested; `--service` isn't available. |

## Next steps

- [Lesson 1](../examples/01-agent-server/README.md) walks through a local agent, pairing and approvals.
- [Lesson 2](../examples/02-tailscale-telegram-grok/README.md) puts the agent on a tailnet with a Telegram bot and a Grok webhook node.
- [Lesson 3](../examples/03-agentmesh-relay/README.md) runs two agents on an Agent Mesh relay.
- The [access model](concepts/access-model.md) explains who can call an agent.
