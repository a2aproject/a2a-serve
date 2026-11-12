# Getting Started

Install a2a-serve, start a coding agent in your project, and talk to it from your terminal and your phone.

## Install

```bash
brew tap a2aproject/a2a-cli https://github.com/a2aproject/a2a-cli
brew install a2aproject/a2a-cli/a2a-serve && a2a-serve doctor --fix
```

Without Homebrew, run the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
```

<details markdown="1">
<summary>Build from <code>main</code> with Go 1.26+</summary>

```bash
CGO_ENABLED=0 go install github.com/a2aproject/a2a-serve@main &&
  go install github.com/a2aproject/a2a-cli@latest &&
  mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a" &&
  a2a-serve doctor --fix
```

To join Agent Mesh relays, build the Agent Mesh network provider next to `a2a-serve` from a clone:

```bash
git clone https://github.com/a2aproject/a2a-serve && cd a2a-serve/providers/agentmesh &&
  CGO_ENABLED=0 go build -o "$(dirname "$(command -v a2a-serve)")/netprovider-agentmesh" .
```

</details>

Every method installs `a2a-serve` and the `a2a` CLI, which you use to talk to agents.

## Start an agent

In your project, start Claude Code (or `opencode`) with its API key:

```bash
cd ~/src/app
a2a-serve start claude --secret ANTHROPIC_API_KEY --detach
```

For `opencode`, run `a2a-serve start opencode --model openrouter/~google/gemini-flash-latest --secret OPENROUTER_API_KEY --detach`. Without Docker, install the agent on your machine and add `--sandbox local`.

## Talk to it

```bash
a2a send -a "$(a2a-serve status --url=local)" "What does this project do?"
```

Ask for something that changes a file:

```bash
a2a send -a "$(a2a-serve status --url=local)" "Write the output of 'uname -sr' to system.txt."
```

The agent stops and asks first. Approve it with the task ID from the output:

```bash
a2a send -a "$(a2a-serve status --url=local)" --task-id <task-id> once
```

## Take it with you

Create a Telegram bot with [@BotFather](https://t.me/BotFather) and connect it:

```bash
a2a-serve messenger add telegram --token env:BOT_TOKEN
```

Open the `t.me` link it prints and press **Start**. Now you can chat with the agent from your phone and approve its commands with a button.

To share the agent, invite someone:

```bash
a2a-serve pair invite anna                   # prints a one-time code
a2a-serve pair join <agent-url> <code>       # anna runs this
```

## Next steps

- [Serve a Coding Agent](../examples/01-agent-server/README.md) covers pairing, approvals, and an agent that calls another agent.
- [Tailscale, Telegram, and Grok](../examples/02-tailscale-telegram-grok/README.md) puts the agent on your tailnet.
- The [user guide](index.md#user-guide) covers every feature.
