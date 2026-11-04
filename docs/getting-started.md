# Getting Started

Install a2a-serve, start a coding agent in your project, and talk to it from your terminal and your phone.

## Install

```bash
brew install a2aproject/a2a-cli/a2a-serve    # macOS and Linux
a2a-serve doctor --fix                       # set up the a2a CLI and check Docker
```

Without Homebrew, use the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/a2aproject/a2a-serve/main/install.sh | sh
```

Both install `a2a-serve` and the `a2a` CLI, which you use to talk to agents.

## Start an agent

In your project, start opencode with your model's API key:

```bash
cd ~/src/app
a2a-serve start opencode --secret OPENROUTER_API_KEY --detach
```

For Claude Code, run `a2a-serve start claude --secret ANTHROPIC_API_KEY --detach`. Without Docker, add `--sandbox local`.

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
a2a send -a "$(a2a-serve status --url=local)" --task-id <task-id> approve
```

## Take it with you

Create a Telegram bot with [@BotFather](https://t.me/BotFather) and connect it:

```bash
a2a-serve channel add telegram --token env:BOT_TOKEN
```

Open the `t.me` link it prints and press **Start**. Now you can chat with the agent from your phone and approve its commands with a button.

To share the agent, invite someone:

```bash
a2a-serve pair invite anna                   # prints a one-time code
a2a-serve pair join <agent-url> <code>       # anna runs this
```

If anything fails, run `a2a-serve doctor`. It checks your setup and prints a fix for each problem.

## Next steps

- [Serve a Coding Agent](../examples/01-agent-server/README.md) covers pairing, approvals and an agent that calls another agent.
- [Tailscale, Telegram and a Grok Bot](../examples/02-tailscale-telegram-grok/README.md) puts the agent on your tailnet with a Telegram bot and a Grok webhook node.
- [Agent Mesh Relay](../examples/03-agentmesh-relay/README.md) runs two agents on an Agent Mesh relay.
- The [user guide](index.md#user-guide) covers every feature.
