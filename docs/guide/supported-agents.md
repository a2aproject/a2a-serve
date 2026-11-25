# Supported Agents

The server runs opencode, Claude Code, GitHub Copilot CLI, Antigravity CLI or any other ACP agent, on Linux and macOS.

| Agent | Image |
| --- | --- |
| `opencode` | `ghcr.io/a2aproject/harness-opencode`, with the latest `opencode-ai` at publish time |
| `claude` | Built on your machine on the first start, with the latest Claude Code and `claude-agent-acp` at build time |
| `copilot` | Built on your machine on the first start, with the latest GitHub Copilot CLI at build time |
| `agy` | Built on your machine on the first start, with the latest Antigravity CLI at build time |
| `acp --command …` | Any agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com), in any image with `--image` |

`--command` also replaces the command of `opencode`, `claude`, `copilot` or `agy`, as in `a2a-serve start opencode --command "opencode acp --print-logs"`. The images include Node.js, `git`, `ripgrep`, `python3`, `npx`, `uvx` and the `a2a` CLI.

| Platform | Status |
| --- | --- |
| Linux (amd64, arm64) with Docker, rootful or rootless | Supported. `--service` installs a systemd user unit. |
| macOS (Apple silicon, Intel) with Docker Desktop | Supported. `--service` installs a launchd agent. |
| Linux without Docker | `--sandbox bwrap` runs the agent installed on this machine with [bubblewrap](sandbox.md#without-docker-on-linux). `--sandbox local` runs it on the host with no isolation. |
| macOS without Docker | `--sandbox local` only: the agent runs on the host with no isolation. |
| Windows | Not supported. Releases include no Windows binaries. |

## GitHub Copilot CLI

Copilot's ACP mode is in public preview, so it can change or break between Copilot releases. Start it with a GitHub token that has the Copilot Requests permission:

```bash
a2a-serve start copilot --secret COPILOT_GITHUB_TOKEN --detach
```

`--model` takes a model that Copilot lists, such as `gpt-6.2-sol`.

The agent works with your Copilot subscription, also for the people and agents you let call it. GitHub's terms don't allow sharing or reselling access to Copilot, and preview features come with their own terms. Read the [GitHub Copilot terms](https://docs.github.com/en/site-policy/github-terms/github-terms-for-additional-products-and-features#github-copilot) before you let guests or paired callers use the agent.

Some features work differently with Copilot:

- It gets only the MCP servers of `.mcp.json` with a URL. Servers with a command don't start.
- `tool:<server>.<tool>` rules don't match its MCP tool calls. Use the `other` kind instead.
- Only you can send it Copilot's own slash commands, such as `/compact`. A message from anyone else that starts with `/` reaches it as text.
- With `--sandbox bwrap --egress unrestricted` or `--sandbox local`, Copilot runs the hooks of its settings and lets a message of yours such as `/allow-all on` turn its approvals off.

## Antigravity CLI

Start it with a Gemini API key from Google AI Studio:

```bash
a2a-serve start agy --secret GEMINI_API_KEY --detach
```

`--model` takes a model that Antigravity CLI lists, such as `gemini-3.6-flash`.

The agent sends its requests to the Gemini API with your key, also for the people and agents you let call it. Google's terms don't allow other tools to use a signed-in Antigravity account, so the server sets the agent up to use the key. Read the [Antigravity FAQ](https://antigravity.google/docs/faq/) before you change that in the agent home directory.

Some features work differently with Antigravity CLI:

- It runs the hooks in the agent home directory, and a hook can approve a tool call without asking you.
- It writes files in its temporary folder, usually `/tmp`, without asking. Keep the workspace outside it.
- With `--sandbox local`, it also runs the hooks of the repository's `.agents/` folder.
