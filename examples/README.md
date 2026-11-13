# Examples

Step-by-step examples that build on each other, from a local agent to a team of agents on a relay.

### 1. Serve a Coding Agent: [`01-agent-server/`](01-agent-server/)

Turn opencode or Claude Code into an A2A agent. Pair with it, send it work, approve a tool call it asks about, and let it call a second agent.

### 2. Tailscale, Telegram and a Grok Bot: [`02-tailscale-telegram-grok/`](02-tailscale-telegram-grok/)

Make the agent reachable from anywhere: on your tailnet with no pairing, on a public Funnel URL, and from a Telegram chat where you approve its tool calls. Then host a Grok bot as a webhook node it can delegate to, and run it as a service.

### 3. Agent Mesh Relay (Experimental): [`03-agentmesh-relay/`](03-agentmesh-relay/)

Run an Agent Mesh relay on your machine and join two agents to it with no Tailscale account. Let one call the other over the relay, and pair a caller at an agent's public URL on the relay.

### 4. Observability Agent: [`04-observability-agent/`](04-observability-agent/)

Create an agent from a recipe that answers questions about production with your metrics, logs, traces and profiles through Grafana. Give it read-only identities, describe your environment, share it with your team, and update it to a newer version of its recipe.

### 5. Relay on Cloud Run: [`05-relay-cloud-run/`](05-relay-cloud-run/)

Run a relay on Google Cloud Run, with its data in Cloud SQL and its secrets in Secret Manager. Invite agents, change its settings, and upgrade it to a newer release.

### 6. Talk to Your Agent on Telegram: [`06-telegram-voice/`](06-telegram-voice/)

Connect the agent to a Telegram bot and give the server a speech service, either an OpenAI key or speaches on your machine. Send voice notes, check what the agent heard, turn on voice replies with `/voice`, and see how the agent card lists audio for A2A callers.

### 7. SLIM Network (Experimental): [`07-slim-network/`](07-slim-network/)

Run a SLIM node in Docker and join two agents to it with the SLIM network provider. Let one call the other over SLIMRPC, and call an agent by its SLIM name from a SLIM client that knows nothing about a2a-serve.
