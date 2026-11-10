# A2A Serve

Turn opencode, Claude Code, or any ACP coding agent into a sandboxed [A2A](https://a2a-protocol.org) agent that people, messengers, and other agents can call.

> [!WARNING]
> A2A Serve is pre-1.0. Commands, flags, and `config.yaml` keys may change; stored state always migrates forward. Read [Security](advanced/security.md) and [Known Limitations](#known-limitations) before exposing an agent to other people.

- **Use your agent from your phone:** message it on [Telegram, Discord, or email](guide/messengers.md) and approve edits or commands from chat.
- **Talk to it with voice notes:** send [voice notes from Telegram, Discord, or email](guide/voice.md) and get voice replies with `/voice on`, through OpenAI or a speech server you run.
- **Share an agent with your team:** expose it on [Tailscale, an Agent Mesh relay, or a public URL](guide/networks.md) with [pairing codes](guide/identities.md) or [Google sign-in](guide/email-sign-in.md).
- **Let agents call other agents:** delegate tasks to [contacts, peers](guide/agent2agent.md), and [webhook nodes](guide/webhook-nodes.md) while keeping tokens outside the sandbox.
- **Package repeatable setups:** bundle instructions, skills, [MCP servers](guide/mcp-and-skills.md), and [schedules](guide/schedules.md) into a one-command [recipe](guide/recipes.md).

<p align="center">
  <img src="graphs/readme-overview.png#gh-light-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
  <img src="graphs/readme-overview-dark.png#gh-dark-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
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

To build from `main` with Go 1.26+, see [Getting Started](getting-started.md#install).

Start an agent in your project directory and send it a task:

```bash
# Claude Code (or: opencode --model <provider/model> --secret <KEY>)
a2a-serve start claude --secret ANTHROPIC_API_KEY --detach
a2a send -a "$(a2a-serve status --url=local)" "What does this project do?"
```

Without Docker, install the agent on your machine and add `--sandbox local`. Follow [Getting Started](getting-started.md) to approve tool calls, connect Telegram, and invite callers.

## Examples

1. **[Serve a Coding Agent](../examples/01-agent-server/README.md):** pair with an agent, approve tool calls, and call a second agent.
2. **[Tailscale, Telegram, and Grok](../examples/02-tailscale-telegram-grok/README.md):** expose the agent on Tailscale and Telegram and add a webhook node.
3. **[Agent Mesh Relay](../examples/03-agentmesh-relay/README.md):** connect two agents through a local relay without Tailscale.
4. **[Observability Agent](../examples/04-observability-agent/README.md):** run a read-only production assistant from a Grafana recipe.
5. **[Relay on Cloud Run](../examples/05-relay-cloud-run/README.md):** deploy a persistent relay with Cloud SQL and Secret Manager.
6. **[Talk to Your Agent on Telegram](../examples/06-telegram-voice/README.md):** send voice notes to the agent and get voice replies.

## User Guide

| Area | Guides |
|---|---|
| **Setup & sandbox** | [Getting Started](getting-started.md) · [Supported Agents](guide/supported-agents.md) · [Running](guide/running.md) · [Config](guide/config.md) · [Secrets](guide/secrets.md) · [Sandbox](guide/sandbox.md) |
| **Extend & operate** | [MCP and Skills](guide/mcp-and-skills.md) · [Recipes](guide/recipes.md) · [Schedules](guide/schedules.md) · [Backups](guide/backups.md) · [Shell Completion](guide/shell-completion.md) |
| **Access & approvals** | [Approvals](guide/approvals.md) · [Inspector](guide/inspector.md) · [Identities](guide/identities.md) · [Email Sign-In](guide/email-sign-in.md) |
| **Networks & chat** | [Networks](guide/networks.md) · [Relays](guide/relays.md) · [Messengers](guide/messengers.md) · [Voice](guide/voice.md) |
| **Multi-agent** | [Agent2Agent](guide/agent2agent.md) · [Webhook Nodes](guide/webhook-nodes.md) |
| **Architecture & reference** | [Architecture](advanced/architecture.md) · [Access Model](advanced/access-model.md) · [Security](advanced/security.md) · [Agent Mesh](advanced/agent-mesh.md) · [Agent Directory](advanced/agent-directory.md) · [Commands](reference/commands/a2a-serve.md) · [Config File](reference/configuration.md) · [HTTP Routes](reference/http-routes.md) · [Approval Messages](reference/approval-messages.md) · [Output](reference/output.md) · [a2a CLI](reference/a2a-cli.md) |

## Known Limitations

- **API keys passed with `--secret` are readable inside the sandbox.** They are environment variables in every agent container that a [sandbox rule](guide/sandbox.md#what-each-callers-container-gets) gives them to. A tricked agent or any caller allowed to approve commands can read the keys their container gets. `--inject-model-key` and `--inject-header` keep a key out of the sandbox, and the server adds it to the agent's HTTPS requests to its host ([Secrets](guide/secrets.md#keep-keys-out-of-the-sandbox)). The agent can still spend such a key through that host, so give it a key with a spending limit ([#235](https://github.com/a2aproject/a2a-serve/issues/235), and [Security](advanced/security.md)).
- **The sandbox can reach any public host.** A tricked agent can send the files it can read to any host on the internet. It can't reach the tailnet or the LAN unless you allow them, and `--egress allowlist` limits it to the hosts you list ([Network](guide/sandbox.md#network)).
- **A cloned repository can configure the agent you start in it.** `a2a-serve start` takes the command, image, mounts, environment variables, and MCP servers from the project's `.a2a/config.yaml` and `.mcp.json`. Read both before starting an agent in a repository you did not write.
- **Agent images follow the latest agent releases** at publish time, so the same tag can carry different agent versions over time.
