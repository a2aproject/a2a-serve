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
> Alpha. Commands, flags and stored state may change.

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
- **Sandboxed by default:** the harness runs in a container that sees the workspace, and none of the server's secrets, tokens or config.
- **Approvals:** tool calls that change something pause the task until an approver answers from the CLI, with Telegram or Discord buttons, or by replying to an email.
- **Reachable:** on localhost, your tailnet, the LAN, Tailscale Funnel or ngrok, and from Telegram, Discord and email.
- **Pairing:** people, scripts and messenger accounts get access with one-time codes, and lose it with `a2a serve pair rm`.
- **Agents calling agents:** the harness calls other agents with `a2a send` through the server, so its credentials stay outside the sandbox. A called agent's approval requests reach you in the chat you are already in.
- **Cloud agents as nodes:** host an agent that only receives webhooks, such as a Grok Bot routine. The server holds its tasks until it wakes.

<div align="center">
  <img src="./docs/graphs/readme-overview.png" alt="Callers reach a2a serve, which runs the coding agent in a Docker sandbox; the agent calls other agents through the server" width="720">
</div>

## Getting Started

`a2a-serve` is a command plugin of the [A2A CLI](https://github.com/a2aproject/a2a-cli): with it on your `PATH`, it runs as `a2a serve`. Install both, then let `doctor` configure the CLI and check Docker:

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
- **[Cookbook](./examples/):** runnable lessons, from [serving a coding agent](./examples/01-agent-server/) to [Tailscale, Telegram and a Grok bot](./examples/02-tailscale-telegram-grok/).

## Contributing

Open an issue or pull request to share suggestions, questions, or edge cases. See the **[Contributing guide](CONTRIBUTING.md)**.

## License

A2A Serve is licensed under the [Apache License 2.0](LICENSE).
