# A2A Serve

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/a2aproject/a2a-serve" alt="License: Apache-2.0"></a>
</p>

<div align="center">
   <img src="https://raw.githubusercontent.com/a2aproject/A2A/refs/heads/main/docs/assets/a2a_logo/color/SVG/a2a_color.svg" width="256" alt="Agent2Agent Protocol Logo"/>
</div>

Visit [Getting Started](https://a2aproject.github.io/a2a-serve/getting-started/) to see how a single command turns the coding agent on your machine into a sandboxed [A2A](https://a2a-protocol.org) agent that popular messengers and other agents can call.

> [!WARNING]
> A2A Serve is pre-1.0. Commands, flags and `config.yaml` keys may change; stored state always migrates forward. Read the [security page](./docs/advanced/security.md) and the [known limitations](#-known-limitations) before you expose an agent to other people.

## ✨ Features

- **Any ACP agent:** opencode, Claude Code, or your own with `a2a-serve start acp --command`.
- **Sandbox and approvals:** the agent runs in Docker, and its edits and commands wait for your approval.
- **Reachable anywhere:** your tailnet, an [Agent Mesh](./docs/advanced/agent-mesh.md) relay, a public URL, Telegram, Discord or email, with access by one-time pairing codes.
- **Agents calling agents:** the agent delegates to other agents through the server, which keeps their tokens outside the sandbox.

<p align="center">
  <img src="./docs/graphs/readme-overview.png#gh-light-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
  <img src="./docs/graphs/readme-overview-dark.png#gh-dark-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
</p>

## 📚 User Guide

| | |
|---|---|
| [Supported Agents](./docs/guide/supported-agents.md) | The supported agents, images and platforms |
| [Running](./docs/guide/running.md) | Start, stop, restart and check on an agent |
| [Config](./docs/guide/config.md) | Change settings |
| [Secrets](./docs/guide/secrets.md) | API keys for the agent and tokens for the server |
| [Sandbox](./docs/guide/sandbox.md) | What the Docker sandbox mounts and limits |
| [MCP and Skills](./docs/guide/mcp-and-skills.md) | Give the agent the MCP servers and skills of its project |
| [Approvals](./docs/guide/approvals.md) | Answer tool calls, and who answers them |
| [Inspector](./docs/guide/inspector.md) | A web page with conversations, tasks and approvals |
| [Identities](./docs/guide/identities.md) | Callers, roles and pairing codes |
| [Email Sign-In](./docs/guide/email-sign-in.md) | Let people on an email allowlist call the agent |
| [Networks](./docs/guide/networks.md) | Tailscale, the local network, Agent Mesh relays and public URLs |
| [Messengers](./docs/guide/messengers.md) | Telegram, Discord and email |
| [Agent2Agent](./docs/guide/agent2agent.md) | Contacts, peers and calls between agents |
| [Schedules](./docs/guide/schedules.md) | Run prompts at set times |
| [Webhook Nodes](./docs/guide/webhook-nodes.md) | Give agents that run only when woken an A2A address |
| [Backups](./docs/guide/backups.md) | Encrypted snapshots and restores on another machine |
| [Shell Completion](./docs/guide/shell-completion.md) | Completion scripts for bash, zsh, fish and PowerShell |

## 🚧 Known Limitations

- **API keys are readable inside the sandbox.** Values passed with `--secret` are environment variables of the agent, so a tricked agent can read them and send them anywhere ([#16](https://github.com/a2aproject/a2a-serve/issues/16)).
- **The sandbox can reach any host.** A tricked agent can send the files it can read anywhere ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
- **A cloned repository can configure the agent you start in it.** `a2a-serve start` takes the command, image, mounts, environment variables and MCP servers from the project's `.a2a/config.yaml` and `.mcp.json`. Read both before you start an agent in a repository you didn't write.
- **Agent images follow the latest agent releases** at the time they were published, so the same tag can carry different agent versions over time.

## 🤝 Contributing

Open an issue or pull request to share suggestions, questions, or edge cases. See the **[Contributing guide](CONTRIBUTING.md)**.

## 📄 License

A2A Serve is licensed under the [Apache License 2.0](LICENSE).
