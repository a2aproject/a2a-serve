# Examples

### 1. Serve a coding agent — [`01-agent-server/`](01-agent-server/)

Turn opencode or Claude Code into an A2A agent with `a2a serve <harness>`. Pair with it, send it work, approve a tool call it asks about, and let it call a second agent.

### 2. Tailscale, Telegram, and a Grok bot — [`02-tailscale-telegram-grok/`](02-tailscale-telegram-grok/)

Make the agent reachable from anywhere: on your tailnet with no pairing, on a public Funnel URL, and from a Telegram chat where you approve its tool calls. Then host a Grok bot as a webhook node it can delegate to, and run it as a service.
