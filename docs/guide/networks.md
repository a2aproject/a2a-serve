# Networks

The agent is reachable on localhost by default. Network flags add a private URL, a public URL or both.

| Flag | Reachable from | Callers identified by |
| --- | --- | --- |
| *(none)* | localhost and messengers | pairing tokens |
| `--private tailscale` | your tailnet, as `https://<name>.<tailnet>.ts.net` | Tailscale identity |
| `--private <relay-url>` | other a2a-serve agents on an [Agent Mesh relay](#agent-mesh-relays-experimental) | the relay's identity |
| `--public tailscale` | the internet through Funnel, at the same URL | pairing tokens |
| `--public <relay-url>` | the internet, at a URL on the relay | pairing tokens |

## Tailscale

Each agent joins your tailnet as its own machine, even when the computer already runs Tailscale. On the first run it prints a login link. On a headless machine, store an auth key with `config set`, then start the agent:

```sh
a2a-serve config set --agent opencode --tailscale-authkey env:TS_AUTHKEY
a2a-serve start opencode --private tailscale
```

`config set --agent` creates the agent without starting it. After the agent joins:

- turn off key expiry for the machine in the admin console;
- enable HTTPS certificates for the tailnet;
- for Funnel, add the `funnel` node attribute to the tailnet policy.

`--tailscale-control-url` points at a Headscale server.

## Agent Mesh relays (experimental)

An agent can join an [Agent Mesh](https://google.github.io/agentmesh/) relay with no Tailscale account. Agents on the relay can call each other from behind NAT, and each can get a public URL:

```sh
a2a-serve start opencode --private https://relay.example.com --public https://relay.example.com
# first run: → sign in to the relay https://relay.example.com: https://…
a2a-serve config set --relay-token env:RELAY_TOKEN    # on servers: join with a token
```

On the first run, open the link and sign in with Google. The relay admits the emails its operator allows. [Relays](relays.md) shows how to run a relay with `a2a-serve relay run`, let people sign in, and invite agents to it.

- A relay works like a tailnet: everyone the relay admits can call your agent. With `--private-allow-incoming paired`, only you and your pairings can call. [Agent Mesh](../advanced/agent-mesh.md#how-the-access-model-maps) explains who a relay admits.
- The private URL works only from other a2a-serve agents on the relay. Give people the public URL.
- The relay URL uses `https://`, since the agent sends its tokens to it. `http://` works only for a relay on the same machine, such as `http://127.0.0.1:8443`.
- Relays need a build with `-tags agentmesh`: `CGO_ENABLED=0 go install -tags agentmesh github.com/a2aproject/a2a-serve@latest`. Release binaries don't include it yet.

The [Agent Mesh relay example](../../examples/03-agentmesh-relay/README.md) runs a relay and two agents on it, and [Agent Mesh](../advanced/agent-mesh.md) explains how the two projects fit together.
