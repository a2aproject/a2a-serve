# Networks

The agent is reachable on localhost by default. Network flags add a private URL, a public URL or both.

| Flag | Reachable from | Callers identified by |
| --- | --- | --- |
| *(none)* | localhost and messengers | pairing tokens |
| `--private tailscale` | your tailnet, as `https://<name>.<tailnet>.ts.net` | Tailscale identity |
| `--private lan` | the local network, over plain HTTP | the caller's address |
| `--private <relay-url>` | other a2a-serve agents on an [Agent Mesh relay](#agent-mesh-relays-experimental) | the relay's identity |
| `--public tailscale` | the internet through Funnel, at the same URL | pairing tokens |
| `--public <relay-url>` | the internet, at a URL on the relay | pairing tokens |

## Tailscale

Each agent joins your tailnet as its own machine, even when the computer already runs Tailscale. On the first run it prints a login link; on a headless machine, use `--tailscale-authkey env:TS_AUTHKEY`. Then:

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

- A relay that admits people by sign-in accepts anyone its identity provider accepts, so on it `--private-access` defaults to `paired`: only you, your pairings and `--allow-email` matches can call. On a relay that agents join only with tokens, everyone on the relay can call.
- The private URL works only from other a2a-serve agents on the relay. Give people the public URL.
- Relays need a build with `-tags agentmesh`: `go install -tags agentmesh github.com/a2aproject/a2a-serve@latest`. Release binaries don't include it yet.

The [Agent Mesh relay example](../../examples/03-agentmesh-relay/README.md) runs two agents on a relay, and [Agent Mesh](../advanced/agent-mesh.md) explains how the two projects fit together.
