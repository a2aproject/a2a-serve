# Networks

The agent is reachable on localhost by default. Network flags add a private URL, a public URL or both.

| Flag | Reachable from | Callers identified by |
| --- | --- | --- |
| *(none)* | localhost and messengers | pairing tokens |
| `--private tailscale` | your tailnet, as `https://<name>.<tailnet>.ts.net` (`http://` with Headscale) | Tailscale identity |
| `--private <relay-url>` | other a2a-serve agents on an [Agent Mesh relay](#agent-mesh-relays-experimental) | the relay's identity |
| `--public tailscale` | the internet through Funnel, at the same URL | pairing tokens |
| `--public <relay-url>` | the internet, at a URL on the relay | pairing tokens |
| `--private <url> --network-provider <name>` | agents on another network, such as a [SLIM node](#slim-nodes-experimental) | the network's identity |

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

`--tailscale-control-url` points at a Headscale server. Headscale issues no HTTPS certificates, so the private URL is `http://<name>.<base domain>`. WireGuard encrypts the traffic when the caller resolves the name with MagicDNS. On a device without MagicDNS, requests with tokens could cross the internet unencrypted, so pick a base domain that doesn't resolve on the internet. Headscale offers no Funnel, so `--public tailscale` needs Tailscale's own control server.

## Agent Mesh relays (experimental)

An agent can join an [Agent Mesh](https://google.github.io/agentmesh/) relay with no Tailscale account. Agents on the relay can call each other from behind NAT, and each can get a public URL:

```sh
a2a-serve start opencode --private https://relay.example.com --public https://relay.example.com
# first run: → sign in to the relay https://relay.example.com: https://…
a2a-serve config set --network-token env:RELAY_TOKEN    # on servers: join with an invite
```

On the first run, open the link and sign in with Google. The relay admits the emails its operator allows. [Relays](relays.md) shows how to run a relay with `a2a-serve relay run`, let people sign in, and invite agents to it.

- Everyone the relay admits can call your agent. With `--private-allow-incoming paired`, only you and your pairings can call. [Agent Mesh](../advanced/agent-mesh.md#how-the-access-model-maps) explains who a relay admits.
- The private URL works only from other a2a-serve agents on the relay. Give people the public URL.
- Release archives include the Agent Mesh network provider. A build from source needs it next to `a2a-serve`; `a2a-serve doctor` prints the command that builds it.

The [Agent Mesh relay example](../../examples/03-agentmesh-relay/README.md) runs a relay and two agents on it, and [Agent Mesh](../advanced/agent-mesh.md) explains how the two projects fit together.

## SLIM nodes (experimental)

An agent can join a [SLIM](https://docs.agntcy.org/slim/overview/) node. Agents on the node call each other over SLIMRPC, and any SLIM A2A client can call the agent by its SLIM name:

```sh
printf '%s' "$SLIM_SECRET" | a2a-serve config set --network-token -
a2a-serve start opencode --private 'http://127.0.0.1:46357?namespace=demo&peers=reviewer' --network-provider slim
#   private  http://slim.local/a2a/demo/coder
```

The URL is the node's address, with settings in the query:

| Setting | Meaning |
| --- | --- |
| `org`, `namespace` | the first two parts of the agent's SLIM name, `<org>/<namespace>/<agent>` (default `a2a/default`) |
| `peers` | the agents `a2a-serve peers` lists, since SLIM can't list them |

The SLIM network provider has no release yet: build it next to `a2a-serve`, as [step 2 of the example](../../examples/07-slim-network/README.md#2-build-the-slim-network-provider) shows. The network token is the shared secret of the node's agents. Anyone with it can claim any name on the node, so use it only on networks of your own machines. SLIM gives agents no public URL.

The [SLIM network example](../../examples/07-slim-network/README.md) runs a node and two agents on it.

## Network providers

A network URL in `--private` or `--public` is joined by a network provider, a program the server runs next to the agent. `--network-provider` picks it: `agentmesh`, the default, or `slim`. The server finds them next to `a2a-serve`. Installing them from releases that a2a-serve pins is planned.

`--network-provider` also takes a release URL or a path, for providers outside that list. The server shows where the provider comes from and its fingerprint, and installs it after you confirm:

```sh
a2a-serve config set --private https://node.example.com --network-provider https://example.com/provider_linux_amd64.tar.gz
# network provider from https://example.com/provider_linux_amd64.tar.gz
#   sha256  9f2c…
# It runs as you, with the agent's network token. Install it? [y/N]
```

`--yes` installs without asking. The agent runs the copy you installed. When you run the command again and the provider at that source changed, it asks again. [Network provider protocol](../reference/network-providers.md) specifies what a provider implements.
