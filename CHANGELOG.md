# Changelog

## 0.1.0 (2026-11-11)

First release. A2A Serve turns opencode, Claude Code or any ACP coding agent into a sandboxed [A2A](https://a2a-protocol.org) agent that people, messengers and other agents can call. See the [documentation](https://a2aproject.github.io/a2a-serve/).

### Features

#### Setup and sandbox

* **[Supported agents](https://a2aproject.github.io/a2a-serve/guide/supported-agents/):** run opencode, Claude Code or any other ACP agent on Linux and macOS.
* **[Running](https://a2aproject.github.io/a2a-serve/guide/running/):** run one agent per project directory, in the foreground, detached or as a user service.
* **[Config](https://a2aproject.github.io/a2a-serve/guide/config/):** keep settings in `.a2a/config.yaml` and change them with `a2a-serve config set` or `config edit`.
* **[Secrets](https://a2aproject.github.io/a2a-serve/guide/secrets/):** store API keys and tokens, and inject model keys and headers so they stay out of the sandbox.
* **[Sandbox](https://a2aproject.github.io/a2a-serve/guide/sandbox/):** run the agent in Docker, with separate containers for members and guests, and storage, secret and egress rules.
* **`a2a-serve doctor`:** check the setup and fix common problems with `--fix`.

#### Extend and operate

* **[MCP and skills](https://a2aproject.github.io/a2a-serve/guide/mcp-and-skills/):** give the agent the MCP servers and skills of its project.
* **[Recipes](https://a2aproject.github.io/a2a-serve/guide/recipes/):** create a ready-made agent from a recipe directory with one command.
* **[Schedules](https://a2aproject.github.io/a2a-serve/guide/schedules/):** run prompts on a cron schedule or at a set time, and send results to chat.
* **[Backups](https://a2aproject.github.io/a2a-serve/guide/backups/):** take encrypted snapshots of an agent and restore it on another machine.
* **[Shell completion](https://a2aproject.github.io/a2a-serve/guide/shell-completion/):** completion scripts for bash, zsh, fish and PowerShell.

#### Access and approvals

* **[Approvals](https://a2aproject.github.io/a2a-serve/guide/approvals/):** approve edits and commands from an A2A client, chat or the inspector, and build rules from "Always allow".
* **[Inspector](https://a2aproject.github.io/a2a-serve/guide/inspector/):** a web page with conversations, tasks, pending approvals and status.
* **[Identities](https://a2aproject.github.io/a2a-serve/guide/identities/):** owner, member and guest roles, with pairing codes for new callers.
* **[Email sign-in](https://a2aproject.github.io/a2a-serve/guide/email-sign-in/):** let people on an email allowlist call the agent with Google sign-in.

#### Networks and chat

* **[Networks](https://a2aproject.github.io/a2a-serve/guide/networks/):** expose the agent on Tailscale, an Agent Mesh relay or a public URL.
* **[Relays](https://a2aproject.github.io/a2a-serve/guide/relays/):** run an Agent Mesh relay that agents join from anywhere, and manage its members.
* **[Messengers](https://a2aproject.github.io/a2a-serve/guide/messengers/):** talk to the agent through Telegram, Discord and email.
* **[Voice](https://a2aproject.github.io/a2a-serve/guide/voice/):** send voice notes and get voice replies through OpenAI, Gemini or your own speech server.

#### Multi-agent

* **[Agent2Agent](https://a2aproject.github.io/a2a-serve/guide/agent2agent/):** let the agent call contacts and peers, with the server holding the credentials.
* **[Webhook nodes](https://a2aproject.github.io/a2a-serve/guide/webhook-nodes/):** give agents that run only when woken an A2A address, and wake them with a signed POST.
