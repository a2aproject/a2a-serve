# A2A Serve

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/a2aproject/a2a-serve" alt="License: Apache-2.0"></a>
</p>

<div align="center">
   <img src="https://raw.githubusercontent.com/a2aproject/A2A/refs/heads/main/docs/assets/a2a_logo/color/SVG/a2a_color.svg" width="256" alt="Agent2Agent Protocol Logo"/>
   <h3>
       One command turns the coding agent in your project into a sandboxed <a href="https://a2a-protocol.org">A2A</a> agent that people, scripts and other agents can call.
   </h3>
</div>

> [!WARNING]
> A2A Serve is pre-1.0 and under active development. Commands, flags and
> `config.yaml` keys may still change; stored state always migrates forward
> (see [upgrades](./docs/upgrades.md)). Read the [security page](./docs/security.md)
> before you expose an agent to other people, and the
> [known limitations](#-known-limitations) below.

---

```bash
cd ~/src/app
a2a-serve start opencode --secret OPENROUTER_API_KEY --private tailscale --detach
a2a-serve channel add telegram --token env:BOT_TOKEN
```

This starts opencode in a Docker sandbox with the project mounted, gives it an HTTPS address on your tailnet, and connects a Telegram bot to it. Open the link it prints to make the chat yours, and edits and shell commands wait for your approval there. Send it work from a terminal, or invite someone else:

```bash
a2a send -a "$(a2a-serve status --url=local)" "What does this project do?"
a2a-serve pair invite anna   # a one-time code; anna runs: a2a-serve pair join <agent-url> <code>
```

The flags are saved to `.a2a/config.yaml`, so the next start is `a2a-serve start`.

## ✨ Features

- **Any ACP agent:** opencode, Claude Code, or your own with `a2a-serve start acp --command`.
- **Sandbox and approvals:** the agent runs in Docker, and its edits and commands wait for your approval.
- **Reachable anywhere:** your tailnet, a public URL, Telegram, Discord or email, with access by one-time pairing codes.
- **Agents calling agents:** the agent delegates to other agents through the server, which keeps their tokens outside the sandbox.

<div align="center">
  <img src="./docs/graphs/readme-overview.png" alt="Callers reach a2a-serve, which runs the coding agent in a Docker sandbox; the agent calls other agents through the server" width="720">
</div>

## 🚀 Getting Started

`a2a-serve` works on its own, and is also a command plugin of the [A2A CLI](https://github.com/a2aproject/a2a-cli): with it on your `PATH`, every `a2a-serve` command also runs as `a2a serve`. The docs write `a2a-serve`. Callers use `a2a` to send tasks, and harnesses use it to call other agents. One step installs both:

```bash
brew install a2aproject/a2a-cli/a2a-serve    # macOS and Linux; brings the a2a formula too
a2a-serve doctor --fix                       # enable the plugin and the credential helper, check Docker
```

Without Homebrew, the install script downloads both release binaries to `~/.local/bin`, checks their checksums, runs `a2a-serve doctor --fix`, and offers to set up an agent in the current directory:

```bash
curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
```

`INSTALL_DIR`, `A2A_SERVE_VERSION` and `A2A_CLI_VERSION` change where and what it installs. Windows builds are attached to each release but untested, so there is no winget package yet.

From source, until an `a2a` release has `--unix-socket` and credential helpers:

```bash
go install github.com/a2aproject/a2a-serve@latest
go install github.com/a2aproject/a2a-cli@v0.3.2-0.20261030153310-f90e4eb628cd
mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"
a2a-serve doctor --fix
```

The pinned `a2a` version is needed until a release has `--unix-socket` and credential helpers. [Installation](./docs/agent-server.md#installation) covers the manual setup and troubleshooting.

## 📚 Documentation

- **[Agent server guide](./docs/agent-server.md):** sandboxing, approvals, networks, messengers, agent-to-agent calls, webhook nodes, schedules, backups, the inspector and the command reference.
- **[Architecture](./docs/agent-server-architecture.md):** how a task flows through the server, with diagrams.
- **[Security](./docs/security.md):** what the sandbox protects and what each token grants.
- **[Upgrades](./docs/upgrades.md):** how stored state migrates between versions.
- **[Cookbook](./examples/):** runnable lessons, from [serving a coding agent](./examples/01-agent-server/) to [Tailscale, Telegram and a Grok bot](./examples/02-tailscale-telegram-grok/) and an [Agent Mesh relay](./examples/03-agentmesh-relay/).

## 💻 Supported Platforms

| Platform | Status |
|---|---|
| Linux (amd64, arm64) with Docker, rootful or rootless | Supported. The harness reaches the server through a Unix socket. `--service` installs a systemd user unit. |
| macOS (Apple silicon, Intel) with Docker Desktop | Supported. Docker Desktop can't mount the socket, so the harness reaches the server over TCP on the host's loopback with a key. `--service` installs a launchd agent. |
| Linux and macOS without Docker | `--sandbox local` only: the harness runs on the host with no isolation. |
| Windows | Builds, but untested; `--service` isn't available. |

Harnesses:

| Harness | Image | Version |
|---|---|---|
| `opencode` | `ghcr.io/a2aproject/harness-opencode` | `opencode-ai`, the latest release when the image was published |
| `claude` | `ghcr.io/a2aproject/harness-claude` | `@zed-industries/claude-agent-acp` and `@anthropic-ai/claude-code`, the latest releases when the image was published |
| `acp --command …` | any, with `--image` | any agent that speaks ACP |

Both images are Debian with Node.js 22, `git`, `ripgrep`, `python3`, and the `a2a` CLI at the version this `a2a-serve` needs, which the image tag names (`:a2a-<version>`). Building from source needs Go 1.26.

## 🚧 Known Limitations

- **API keys are accessible inside the sandbox.** Values passed with `--secret` are environment variables of the harness, so a tricked agent can read them and send them anywhere ([#16](https://github.com/a2aproject/a2a-serve/issues/16)).
- **The sandbox can reach any host.** A tricked agent can send the files it can read anywhere ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **A cloned repository can configure the agent you start in it.** `a2a-serve start` takes the command, image, mounts, environment variables and MCP servers from the project's `.a2a/config.yaml` and `.mcp.json`. Read both before you start an agent in a repository you didn't write.
- **Harness images follow the latest harness releases** at the time they were published, so the same tag can carry different harness versions over time.

## 🤝 Contributing

Open an issue or pull request to share suggestions, questions, or edge cases. See the **[Contributing guide](CONTRIBUTING.md)**.

## 📄 License

A2A Serve is licensed under the [Apache License 2.0](LICENSE).
