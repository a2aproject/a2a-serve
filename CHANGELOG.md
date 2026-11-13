# Changelog

## [0.2.0](https://github.com/a2aproject/a2a-serve/compare/v0.1.0...v0.2.0) (2026-11-13)


### Features

* add the network provider protocol, its Go SDK, host and conformance suite ([#500](https://github.com/a2aproject/a2a-serve/issues/500)) ([a9d8ce1](https://github.com/a2aproject/a2a-serve/commit/a9d8ce17fcd06ccf7f8cac62bae70db653653984))
* add the SLIM network provider ([#503](https://github.com/a2aproject/a2a-serve/issues/503)) ([6e9bd85](https://github.com/a2aproject/a2a-serve/commit/6e9bd85493ec7c23b6256d591ca1220435b7f20d))
* find, download and install network providers by fingerprint ([#501](https://github.com/a2aproject/a2a-serve/issues/501)) ([62ff755](https://github.com/a2aproject/a2a-serve/commit/62ff7557be6eba6569e9ddea213e9bce4a364d91))
* run a SLIM relay with `relay run --backend slim` ([#497](https://github.com/a2aproject/a2a-serve/issues/497)) ([8feadde](https://github.com/a2aproject/a2a-serve/commit/8feadde8bbad7ae6ef151683ce80d4c8448decde))
* run networks in network providers, and move Agent Mesh to its own module ([#502](https://github.com/a2aproject/a2a-serve/issues/502)) ([fd17a52](https://github.com/a2aproject/a2a-serve/commit/fd17a522962a6f7a6f23e2ae87d2d77b5ed79a8e))


### Bug Fixes

* keep a tunneled request's body open while the response streams, so a peer's stream isn't cut ([#516](https://github.com/a2aproject/a2a-serve/issues/516)) ([6f86b15](https://github.com/a2aproject/a2a-serve/commit/6f86b15d8eb9411149feaba0f3a987602c42c39b))
* retry starting a network provider whose binary is still busy ([#510](https://github.com/a2aproject/a2a-serve/issues/510)) ([b0c01cf](https://github.com/a2aproject/a2a-serve/commit/b0c01cf578fd705f953203b7efd5d8324ac240b8))

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
