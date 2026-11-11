# Supported Agents

The server runs opencode, Claude Code or any other ACP agent, on Linux and macOS.

| Agent | Image |
| --- | --- |
| `opencode` | `ghcr.io/a2aproject/harness-opencode`, with the latest `opencode-ai` at publish time |
| `claude` | `ghcr.io/a2aproject/harness-claude`, with the latest Claude Code and `claude-agent-acp` at publish time |
| `acp --command …` | Any agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com), in any image with `--image` |

`--command` also replaces the command of `opencode` or `claude`, as in `a2a-serve start opencode --command "opencode acp --print-logs"`. The images include Node.js, `git`, `ripgrep`, `python3`, `npx`, `uvx` and the `a2a` CLI.

| Platform | Status |
| --- | --- |
| Linux (amd64, arm64) with Docker, rootful or rootless | Supported. `--service` installs a systemd user unit. |
| macOS (Apple silicon, Intel) with Docker Desktop | Supported. `--service` installs a launchd agent. |
| Linux and macOS without Docker | `--sandbox local` only: the agent runs on the host with no isolation. |
| Windows | Builds, but untested. `--service` isn't available. |
