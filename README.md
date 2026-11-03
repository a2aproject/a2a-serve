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
> [known limitations](#known-limitations) below.

---

```bash
cd ~/src/app
a2a serve opencode --secret OPENROUTER_API_KEY --private tailscale --telegram env:BOT_TOKEN
```

This starts opencode in a Docker sandbox with the project mounted, gives it an HTTPS address on your tailnet, and connects a Telegram bot to it. Open the link it prints to make the chat yours, and edits and shell commands wait for your approval there. Send it work from a terminal, or invite someone else:

```bash
a2a send -a "$(a2a serve url --local)" "What does this project do?"
a2a serve pair invite anna   # a one-time code; anna runs: a2a serve tokens add <agent-url> <code>
```

The flags are saved to `.a2a/config.yaml`, so the next start is `a2a serve`.

## Features

- **Any ACP harness:** opencode, Claude Code, or any [Agent Client Protocol](https://agentclientprotocol.com) agent with `a2a serve acp --command "<cmd>"`.
- **Sandboxed by default:** the harness runs in a container that sees the workspace and the MCP servers and skills of the project, and none of the server's secrets, tokens or config.
- **Approvals:** tool calls that change something pause the task until an approver answers from the CLI, with Telegram or Discord buttons, or by replying to an email.
- **Reachable:** on localhost, your tailnet, the LAN, Tailscale Funnel, ngrok or an Agent Mesh relay (experimental), and from Telegram, Discord and email.
- **Pairing:** people, scripts and messenger accounts get access with one-time codes or by signing in with Google, and lose it with `a2a serve pair rm`.
- **Agents calling agents:** the harness calls other agents with `a2a send` through the server, so its credentials stay outside the sandbox. A called agent's approval requests reach you in the chat you are already in.
- **Cloud agents as nodes:** host an agent that only receives webhooks, such as a Grok Bot routine. The server holds its tasks until it wakes.
- **Inspector:** a web page for the owner with conversations, tasks and pending approvals, opened with `a2a serve open`.
- **Schedules:** prompts that run on a cron expression or once, created by the owner or by the agent itself.
- **Backups:** encrypted snapshots of the agent's state to a directory, Cloud Storage or S3, to restore it on another host.

<div align="center">
  <img src="./docs/graphs/readme-overview.png" alt="Callers reach a2a serve, which runs the coding agent in a Docker sandbox; the agent calls other agents through the server" width="720">
</div>

## Getting Started

`a2a-serve` is a command plugin of the [A2A CLI](https://github.com/a2aproject/a2a-cli): with it on your `PATH`, it runs as `a2a serve`. Callers use `a2a` to send tasks, and harnesses use it to call other agents. One step installs both:

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

## Documentation

- **[Agent server guide](./docs/agent-server.md):** sandboxing, approvals, networks, messengers, agent-to-agent calls, webhook nodes and the command reference.
- **[Architecture](./docs/agent-server-architecture.md):** how a task flows through the server, with diagrams.
- **[Security](./docs/security.md):** what the sandbox protects and what each token grants.
- **[Upgrades](./docs/upgrades.md):** how stored state migrates between versions.
- **[Cookbook](./examples/):** runnable lessons, from [serving a coding agent](./examples/01-agent-server/) to [Tailscale, Telegram and a Grok bot](./examples/02-tailscale-telegram-grok/) and an [Agent Mesh relay](./examples/03-agentmesh-relay/).

## Supported Platforms

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

## Known Limitations

- **API keys are visible inside the sandbox.** Values passed with `--secret` are environment variables of the harness, so a tricked agent can send them anywhere ([#16](https://github.com/a2aproject/a2a-serve/issues/16)).
- **The sandbox can reach any host.** A tricked agent can send the files it can read anywhere ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **A project's files decide what runs.** `config.yaml` and `.mcp.json` pick the command, image, mounts, variables and MCP servers; read them in a repository you didn't write before you start an agent from it.
- **Harness images follow the latest harness releases** at the time they were published, so the same tag can carry different harness versions over time.

## Contributing

Open an issue or pull request to share suggestions, questions, or edge cases. See the **[Contributing guide](CONTRIBUTING.md)**.

## License

A2A Serve is licensed under the [Apache License 2.0](LICENSE).
