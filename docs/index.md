# A2A Serve

One command turns the coding agent in your project into a sandboxed [A2A](https://a2a-protocol.org) agent that people, scripts and other agents can call.

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

> [!WARNING]
> A2A Serve is pre-1.0. Commands, flags and `config.yaml` keys may still change; stored state always migrates forward. Read the [security page](concepts/security.md) before you expose an agent to other people.

## Features

- **Any ACP agent:** opencode, Claude Code, or your own with `a2a-serve start acp --command`.
- **Sandbox and approvals:** the agent runs in Docker, and its edits and commands wait for your approval.
- **Reachable anywhere:** your tailnet, an [Agent Mesh](concepts/agent-mesh.md) relay, a public URL, Telegram, Discord or email, with access by one-time pairing codes.
- **Agents calling agents:** the agent delegates to other agents through the server, which keeps their tokens outside the sandbox.

![Callers reach a2a-serve, which runs the coding agent in a Docker sandbox; the agent calls other agents through the server](graphs/readme-overview.png#gh-light-mode-only)
![Callers reach a2a-serve, which runs the coding agent in a Docker sandbox; the agent calls other agents through the server](graphs/readme-overview-dark.png#gh-dark-mode-only)

## Documentation

| | |
|---|---|
| [Getting started](getting-started.md) | Install a2a-serve and start a first agent |
| [Concepts](concepts/access-model.md) | Who can call an agent, how it runs on Agent Mesh, the architecture and security |
| [User guide](guide.md) | Sandbox, approvals, networks, messengers, agents calling agents, schedules, backups |
| [Command reference](reference/commands/a2a-serve.md) | Every command and flag, the configuration file, exit codes and HTTP routes |
| [Examples](../examples/README.md) | Runnable lessons, from a local agent to an Agent Mesh relay |
