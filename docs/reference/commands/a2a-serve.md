# a2a-serve

<!-- Generated from the command definitions; do not edit. -->

Run a coding agent as an A2A server: one agent per process, started from its
project directory, sandboxed in Docker, reachable over A2A on localhost, a
private network (Tailscale, an Agent Mesh relay) and the internet
(Funnel, a relay), and from Telegram, Discord and email.

```
a2a-serve start opencode --name coder     first run in a project: saves .a2a/config.yaml
a2a-serve start                           later runs, from the project or a subdirectory
```

Management commands (pair, status, contact, ...) find the agent the way
git finds a repository, or take --dir.

## Run the agent

| Command | Description |
|---|---|
| [`inspect`](inspect.md) | Open the web inspector of the agent |
| [`logs`](logs.md) | Print the agent's log |
| [`restart`](restart.md) | Stop the agent and start it again in the same mode, detached or as a service |
| [`start`](start.md) | Start the agent, or a local test server |
| [`status`](status.md) | Show whether the agent runs, its URLs, channels and who may call it, or print one of its URLs |
| [`stop`](stop.md) | Stop the agent |

## Configure the agent

| Command | Description |
|---|---|
| [`backup`](backup.md) | Take, list and restore encrypted backup snapshots of the agent |
| [`channel`](channel.md) | Manage the messengers the agent is reachable from |
| [`config`](config.md) | Show and change the agent's settings |
| [`schedule`](schedule.md) | Run prompts on a schedule |

## Pairing and contacts

| Command | Description |
|---|---|
| [`contact`](contact.md) | Manage the agents this agent may call |
| [`pair`](pair.md) | Pair callers and webhook nodes with the agent, or join another agent |
| [`peers`](peers.md) | List agents found on the private network, and the pairings they advertise |

## Webhook node

| Command | Description |
|---|---|
| [`mailbox`](mailbox.md) | Answer tasks as a webhook node hosted by an agent server |

## Relay

| Command | Description |
|---|---|
| [`relay`](relay.md) | Run an Agent Mesh relay that agents join from anywhere |

## Setup

| Command | Description |
|---|---|
| [`completion`](completion.md) | Print a shell completion script |
| [`doctor`](doctor.md) | Check the setup of this machine and of the agent, and fix what can be fixed locally |
| [`version`](version.md) | Print version information |

## Global flags

Every command takes these.

| Flag | Default | Description |
|---|---|---|
| `-o, --output string` | `text` | Output format: text, json (indented), or jsonl (one compact JSON object per line); config show also takes yaml |
| `-v, --verbose` |  | Print debug logs and progress details to stderr |
