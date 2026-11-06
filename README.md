<div align="center">
  <img src="https://raw.githubusercontent.com/a2aproject/A2A/refs/heads/main/docs/assets/a2a_logo/color/SVG/a2a_color.svg" width="112" alt="Agent2Agent Protocol Logo"/>
</div>

# A2A Serve

<p align="center">
  <a href="https://a2aproject.github.io/a2a-serve/"><img src="https://img.shields.io/badge/docs-a2aproject.github.io-14b8a6" alt="Documentation"></a>
  <a href="https://github.com/a2aproject/a2a-serve/actions/workflows/go.yaml"><img src="https://github.com/a2aproject/a2a-serve/actions/workflows/go.yaml/badge.svg" alt="CI"></a>
  <a href="https://github.com/a2aproject/a2a-serve/actions/workflows/nightly.yaml"><img src="https://github.com/a2aproject/a2a-serve/actions/workflows/nightly.yaml/badge.svg" alt="Nightly"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/a2aproject/a2a-serve" alt="License: Apache-2.0"></a>
</p>

Run opencode, Claude Code, or any ACP coding agent in a Docker sandbox that people, messengers, and other agents can call over [A2A](https://a2a-protocol.org).

> [!WARNING]
> A2A Serve is pre-1.0. Commands, flags and `config.yaml` keys may change; stored state always migrates forward. Read the [security page](./docs/advanced/security.md) and the [known limitations](#known-limitations) before you expose an agent to other people.

## What you can do with it

- **Work with your agent from your phone.** Connect [Telegram, Discord, or email](./docs/guide/messengers.md) to message the agent in your repository and approve file edits or shell commands from chat.
- **Share a project or operations agent with your team.** Put an agent on [Tailscale, an Agent Mesh relay, or a public URL](./docs/guide/networks.md) with [one-time pairing codes](./docs/guide/identities.md) or [Google sign-in](./docs/guide/email-sign-in.md). Each paired or signed-in caller gets their own container.
- **Let agents call other agents.** The agent delegates tasks to [contacts, peers](./docs/guide/agent2agent.md), and [webhook nodes](./docs/guide/webhook-nodes.md) through the server with the `a2a` CLI. Outbound tokens stay outside the sandbox, and approvals return to the person who started the task.
- **Reuse agent setups as recipes.** Package instructions, skills, [MCP servers](./docs/guide/mcp-and-skills.md), and [schedules](./docs/guide/schedules.md) into a [recipe](./docs/guide/recipes.md) anyone can start in one command, such as a read-only [observability agent](./examples/04-observability-agent/README.md) connected to Grafana.

<p align="center">
  <img src="./docs/graphs/readme-overview.png#gh-light-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
  <img src="./docs/graphs/readme-overview-dark.png#gh-dark-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
</p>

## Install

Homebrew (macOS and Linux):

```bash
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli
brew install a2aproject/a2a-cli/a2a-serve
a2a-serve doctor --fix    # set up the a2a CLI and check Docker
```

Install script:

```bash
curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
```

From source with Go 1.26+:

```bash
go install github.com/a2aproject/a2a-serve@latest
go install github.com/a2aproject/a2a-cli@latest && mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"
a2a-serve doctor --fix
```

Homebrew and the install script are planned for the first release, `v0.1.0`. Until then, and to run `main`, install from source with Go 1.26+. Every method installs `a2a-serve` and the `a2a` CLI, which callers use to send tasks.

## Quickstart

In your project directory, start opencode with your model and its API key:

```bash
cd ~/src/app
a2a-serve start opencode --model openrouter/~google/gemini-flash-latest --secret OPENROUTER_API_KEY --detach
```

For Claude Code, run `a2a-serve start claude --secret ANTHROPIC_API_KEY --detach`. Without Docker, add `--sandbox local`.

Send a task from your terminal:

```bash
a2a send -a "$(a2a-serve status --url=local)" "What does this project do?"
```

See [Getting Started](./docs/getting-started.md) to approve tool calls, set up Telegram, and pair with your agent.

## Examples

| Example | What you build |
|---|---|
| [01. Serve a Coding Agent](./examples/01-agent-server/README.md) | Turn opencode or Claude Code into an A2A agent, pair with it, approve tool calls, and call a second agent. |
| [02. Tailscale, Telegram, and Grok](./examples/02-tailscale-telegram-grok/README.md) | Reach the agent over Tailscale, a public Funnel URL, and Telegram, and delegate to a Grok bot webhook node. |
| [03. Agent Mesh Relay](./examples/03-agentmesh-relay/README.md) | Run a relay on your machine, connect two agents without Tailscale, and call between them. |
| [04. Observability Agent](./examples/04-observability-agent/README.md) | Create a production assistant from a recipe that queries metrics, logs, and traces through Grafana. |
| [05. Relay on Cloud Run](./examples/05-relay-cloud-run/README.md) | Deploy a production relay to Google Cloud Run with Cloud SQL storage and Secret Manager tokens. |

## User Guide

### Running and Configuring

| Topic | What it covers |
|---|---|
| [Supported Agents](./docs/guide/supported-agents.md) | Supported coding agents, Docker images, and platforms |
| [Running](./docs/guide/running.md) | Start, stop, restart, inspect status, and show agent cards |
| [Config](./docs/guide/config.md) | Read and update persistent settings |
| [Secrets](./docs/guide/secrets.md) | Manage API keys for the agent and tokens for the server |
| [Sandbox](./docs/guide/sandbox.md) | Docker sandbox mounts, resource limits, and isolation |
| [MCP and Skills](./docs/guide/mcp-and-skills.md) | Connect project MCP servers and reusable skills |
| [Recipes](./docs/guide/recipes.md) | Create, share, and run agents from ready-made recipes |

### Access and Approvals

| Topic | What it covers |
|---|---|
| [Approvals](./docs/guide/approvals.md) | Interactive tool call approval prompts and auto-approval rules |
| [Inspector](./docs/guide/inspector.md) | Web dashboard for live conversations, tasks, and approvals |
| [Identities](./docs/guide/identities.md) | Callers, roles, permissions, and one-time pairing codes |
| [Email Sign-In](./docs/guide/email-sign-in.md) | Allowlist Google accounts for browser sign-in |

### Networks, Messengers, and Multi-Agent

| Topic | What it covers |
|---|---|
| [Networks](./docs/guide/networks.md) | Expose agents on Tailscale, Agent Mesh relays, and public URLs |
| [Relays](./docs/guide/relays.md) | Host an Agent Mesh relay that connects agents across networks |
| [Messengers](./docs/guide/messengers.md) | Chat with the agent over Telegram, Discord, and email |
| [Agent2Agent](./docs/guide/agent2agent.md) | Contacts, peers, agent cards, and calls between agents |
| [Webhook Nodes](./docs/guide/webhook-nodes.md) | Give stateless or serverless agents an A2A address |

### Operations

| Topic | What it covers |
|---|---|
| [Schedules](./docs/guide/schedules.md) | Run prompts on cron schedules, `@every` intervals, or once at a set time |
| [Backups](./docs/guide/backups.md) | Create encrypted snapshots and restore them on another host |
| [Shell Completion](./docs/guide/shell-completion.md) | Set up completions for bash, zsh, fish, and PowerShell |

### Architecture and Reference

| Reference | What it covers |
|---|---|
| [Architecture](./docs/advanced/architecture.md) | Components, pairing, request turns, and agent-to-agent calls |
| [Access Model](./docs/advanced/access-model.md) | Entities, roles, policy bindings, and who may call or approve tasks |
| [Security](./docs/advanced/security.md) | Sandbox boundaries, caller limits, and stored tokens and secrets |
| [Agent Mesh](./docs/advanced/agent-mesh.md) | Joining and running an Agent Mesh relay, and how the access model maps |
| [Agent Directory](./docs/advanced/agent-directory.md) | Files and state stored in `.a2a/` for agents and relays |
| [Command Reference](./docs/reference/commands/a2a-serve.md) | Every CLI command, subcommand, and flag |
| [Configuration File](./docs/reference/configuration.md) | Complete `config.yaml` schema and default values |
| [HTTP Routes](./docs/reference/http-routes.md) | HTTP endpoints exposed by the server and relay |
| [Output and Exit Codes](./docs/reference/output.md) | JSON and text output conventions and exit statuses |
| [The a2a CLI](./docs/reference/a2a-cli.md) | How the server integrates with the `a2a` CLI |

## Known Limitations

- **API keys are readable inside the sandbox.** Values passed with `--secret` are environment variables in every agent container, including the containers of callers and the shared container of anonymous callers. A tricked agent or any caller allowed to run and approve commands can read them ([#16](https://github.com/a2aproject/a2a-serve/issues/16), [#235](https://github.com/a2aproject/a2a-serve/issues/235), and [Security](./docs/advanced/security.md)).
- **The sandbox can reach any host.** A tricked agent can send the files it can read anywhere ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **A cloned repository can configure the agent you start in it.** `a2a-serve start` takes the command, image, mounts, environment variables and MCP servers from the project's `.a2a/config.yaml` and `.mcp.json`. Read both before you start an agent in a repository you didn't write.
- **Agent images follow the latest agent releases** at the time they were published, so the same tag can carry different agent versions over time.

## Contributing

Open an issue or pull request to share suggestions, questions, or edge cases. See the [Contributing guide](CONTRIBUTING.md) and [Code of Conduct](CODE_OF_CONDUCT.md).

## License

A2A Serve is licensed under the [Apache License 2.0](LICENSE).
