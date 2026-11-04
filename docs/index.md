# A2A Serve

Visit [Getting Started](getting-started.md) to see how a single command turns the coding agent on your machine into a sandboxed [A2A](https://a2a-protocol.org) agent that popular messengers and other agents can call.

> [!WARNING]
> A2A Serve is pre-1.0. Commands, flags and `config.yaml` keys may change; stored state always migrates forward. Read the [security page](advanced/security.md) before you expose an agent to other people.

## Features

- **Any ACP agent:** opencode, Claude Code, or your own with `a2a-serve start acp --command`.
- **Sandbox and approvals:** the agent runs in Docker, and its edits and commands wait for your approval.
- **Reachable anywhere:** your tailnet, an [Agent Mesh](advanced/agent-mesh.md) relay, a public URL, Telegram, Discord or email, with access by one-time pairing codes.
- **Agents calling agents:** the agent delegates to other agents through the server, which keeps their tokens outside the sandbox.

<p align="center">
  <img src="graphs/readme-overview.png#gh-light-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
  <img src="graphs/readme-overview-dark.png#gh-dark-mode-only" alt="Callers reach a2a-serve, which runs the coding agent in a sandbox; the agent calls other agents through the server" width="680">
</p>

## User Guide

| | |
|---|---|
| [Supported Agents](guide/supported-agents.md) | The supported agents, images and platforms |
| [Running](guide/running.md) | Start, stop, restart and check on an agent |
| [Config](guide/config.md) | Change settings |
| [Secrets](guide/secrets.md) | API keys for the agent and tokens for the server |
| [Sandbox](guide/sandbox.md) | What the Docker sandbox mounts and limits |
| [MCP and Skills](guide/mcp-and-skills.md) | Give the agent the MCP servers and skills of its project |
| [Approvals](guide/approvals.md) | Answer tool calls, and who answers them |
| [Inspector](guide/inspector.md) | A web page with conversations, tasks and approvals |
| [Identities](guide/identities.md) | Callers, roles and pairing codes |
| [Email Sign-In](guide/email-sign-in.md) | Let people on an email allowlist call the agent |
| [Networks](guide/networks.md) | Tailscale, the local network, Agent Mesh relays and public URLs |
| [Messengers](guide/messengers.md) | Telegram, Discord and email |
| [Agent2Agent](guide/agent2agent.md) | Contacts, peers and calls between agents |
| [Schedules](guide/schedules.md) | Run prompts at set times |
| [Webhook Nodes](guide/webhook-nodes.md) | Give agents that run only when woken an A2A address |
| [Backups](guide/backups.md) | Encrypted snapshots and restores on another machine |
| [Shell Completion](guide/shell-completion.md) | Completion scripts for bash, zsh, fish and PowerShell |
