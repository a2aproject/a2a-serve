# A2A Serve

<div align="center">
  <img src="https://raw.githubusercontent.com/a2aproject/A2A/refs/heads/main/docs/assets/a2a_logo/color/SVG/a2a_color.svg" width="200" alt="Agent2Agent Protocol Logo"/>
</div>

<p align="center">
  <a href="https://a2aproject.github.io/a2a-serve/"><img src="https://img.shields.io/badge/docs-a2aproject.github.io-14b8a6" alt="Documentation"></a>
  <a href="https://github.com/a2aproject/a2a-serve/actions/workflows/go.yaml"><img src="https://github.com/a2aproject/a2a-serve/actions/workflows/go.yaml/badge.svg?branch=main" alt="CI"></a>
  <a href="https://github.com/a2aproject/a2a-serve/actions/workflows/nightly.yaml"><img src="https://github.com/a2aproject/a2a-serve/actions/workflows/nightly.yaml/badge.svg?branch=main" alt="Nightly"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/a2aproject/a2a-serve" alt="License: Apache-2.0"></a>
</p>

Turn opencode, Claude Code, or any ACP coding agent into a sandboxed [A2A](https://a2a-protocol.org) agent that people, messengers, and other agents can call.

> [!WARNING]
> A2A Serve is pre-1.0. Commands, flags, and `config.yaml` keys may change; stored state always migrates forward. Read [Security](./docs/advanced/security.md) and [Known Limitations](#known-limitations) before exposing an agent to other people.

- **Use your agent from your phone:** message it on [Telegram, Discord, or email](./docs/guide/messengers.md) and approve edits or commands from chat.
- **Share an agent with your team:** expose it on [Tailscale, an Agent Mesh relay, or a public URL](./docs/guide/networks.md) with [pairing codes](./docs/guide/identities.md) or [Google sign-in](./docs/guide/email-sign-in.md).
- **Let agents call other agents:** delegate tasks to [contacts, peers](./docs/guide/agent2agent.md), and [webhook nodes](./docs/guide/webhook-nodes.md) while keeping tokens outside the sandbox.
- **Package repeatable setups:** bundle instructions, skills, [MCP servers](./docs/guide/mcp-and-skills.md), and [schedules](./docs/guide/schedules.md) into a one-command [recipe](./docs/guide/recipes.md).

<p align="center">
  <img src="./docs/graphs/readme-overview.png#gh-light-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
  <img src="./docs/graphs/readme-overview-dark.png#gh-dark-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
</p>

## Quickstart

Install `a2a-serve` and the `a2a` CLI on macOS or Linux:

```bash
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli
brew install a2aproject/a2a-cli/a2a-serve && a2a-serve doctor --fix
```

Without Homebrew:

```bash
curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
```

To build from `main` with Go 1.26+ (required before `v0.1.0` is released), see [Getting Started](./docs/getting-started.md#install).

Start an agent in your project directory and send it a task:

```bash
# Claude Code (or: opencode --model <provider/model> --secret <KEY>)
a2a-serve start claude --secret ANTHROPIC_API_KEY --detach
a2a send -a "$(a2a-serve status --url=local)" "What does this project do?"
```

Without Docker, install the agent on your machine and add `--sandbox local`. Follow [Getting Started](./docs/getting-started.md) to approve tool calls, connect Telegram, and invite callers.

## Examples

1. **[Serve a Coding Agent](./examples/01-agent-server/README.md):** pair with an agent, approve tool calls, and call a second agent.
2. **[Tailscale, Telegram, and Grok](./examples/02-tailscale-telegram-grok/README.md):** expose the agent on Tailscale and Telegram and add a webhook node.
3. **[Agent Mesh Relay](./examples/03-agentmesh-relay/README.md):** connect two agents through a local relay without Tailscale.
4. **[Observability Agent](./examples/04-observability-agent/README.md):** run a read-only production assistant from a Grafana recipe.
5. **[Relay on Cloud Run](./examples/05-relay-cloud-run/README.md):** deploy a persistent relay with Cloud SQL and Secret Manager.

## User Guide

| Area | Guides |
|---|---|
| **Setup & sandbox** | [Getting Started](./docs/getting-started.md) · [Supported Agents](./docs/guide/supported-agents.md) · [Running](./docs/guide/running.md) · [Config](./docs/guide/config.md) · [Secrets](./docs/guide/secrets.md) · [Sandbox](./docs/guide/sandbox.md) |
| **Extend & operate** | [MCP and Skills](./docs/guide/mcp-and-skills.md) · [Recipes](./docs/guide/recipes.md) · [Schedules](./docs/guide/schedules.md) · [Backups](./docs/guide/backups.md) · [Shell Completion](./docs/guide/shell-completion.md) |
| **Access & approvals** | [Approvals](./docs/guide/approvals.md) · [Inspector](./docs/guide/inspector.md) · [Identities](./docs/guide/identities.md) · [Email Sign-In](./docs/guide/email-sign-in.md) |
| **Networks & chat** | [Networks](./docs/guide/networks.md) · [Relays](./docs/guide/relays.md) · [Messengers](./docs/guide/messengers.md) |
| **Multi-agent** | [Agent2Agent](./docs/guide/agent2agent.md) · [Webhook Nodes](./docs/guide/webhook-nodes.md) |
| **Architecture & reference** | [Architecture](./docs/advanced/architecture.md) · [Access Model](./docs/advanced/access-model.md) · [Security](./docs/advanced/security.md) · [Agent Mesh](./docs/advanced/agent-mesh.md) · [Agent Directory](./docs/advanced/agent-directory.md) · [Commands](./docs/reference/commands/a2a-serve.md) · [Config File](./docs/reference/configuration.md) · [HTTP Routes](./docs/reference/http-routes.md) · [Output](./docs/reference/output.md) · [a2a CLI](./docs/reference/a2a-cli.md) |

## Known Limitations

- **API keys are readable inside the sandbox.** Values passed with `--secret` are environment variables in every agent container, including caller and anonymous containers. A tricked agent or any caller allowed to approve commands can read them ([#16](https://github.com/a2aproject/a2a-serve/issues/16), [#235](https://github.com/a2aproject/a2a-serve/issues/235), and [Security](./docs/advanced/security.md)).
- **The sandbox can reach any public host.** A tricked agent can send the files it can read to any host on the internet. It can't reach the tailnet or the LAN unless you allow them, and `--egress allowlist` limits it to the hosts you list ([Network](./docs/guide/sandbox.md#network)). With Docker Desktop it still reaches every network.
- **A cloned repository can configure the agent you start in it.** `a2a-serve start` takes the command, image, mounts, environment variables, and MCP servers from the project's `.a2a/config.yaml` and `.mcp.json`. Read both before starting an agent in a repository you did not write.
- **Agent images follow the latest agent releases** at publish time, so the same tag can carry different agent versions over time.

## Contributing

Open an issue or pull request to share suggestions, questions, or edge cases. See the [Contributing guide](CONTRIBUTING.md) and [Code of Conduct](CODE_OF_CONDUCT.md).

## License

A2A Serve is licensed under the [Apache License 2.0](LICENSE).
