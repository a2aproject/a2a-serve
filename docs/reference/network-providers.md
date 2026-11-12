# Network provider protocol

This page specifies how the server runs a network provider and talks to it, for authors of providers in any language. A Go provider can use the SDK in [`netprovider`](https://github.com/a2aproject/a2a-serve/tree/main/netprovider) instead, which implements all of it. [Networks](../guide/networks.md#network-providers) covers how users pick a provider.

## Process

The server starts the provider binary once per agent as `<binary> serve`, with:

- a working directory that only the server's user can enter, the *run directory*;
- an environment of only `PATH`, `HOME`, `USER`, `LOGNAME`, `TMPDIR`, `LANG`, `LC_ALL`, `TZ`, the `XDG_*_HOME` directories, `SSL_CERT_FILE`, `SSL_CERT_DIR` and the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, in both cases);
- stdin and stdout for the protocol, and stderr for logs, which the server writes to its log.

The provider exits when stdin reaches EOF. The server waits 10 seconds after closing stdin, then kills it.

## Messages

Stdin and stdout carry [JSON-RPC 2.0](https://www.jsonrpc.org/specification) messages, one JSON object per line, each at most 1 MiB. The server sends requests; the provider answers each one and sends notifications. Requests can overlap: answer them in any order, matched by `id`.

| Method | Params | Result |
|---|---|---|
| `describe` | none | `{"name", "version", "protocol": 1, "hosts": [...]}` |
| `join` | `{"agent", "description", "url", "token", "state_dir", "private", "public"}` | `{"node", "owner"}` |
| `listen` | `{"private_socket", "public_socket", "token", "connect_socket"}` | `{"private_url", "public_url", "private_note", "connect_token"}` |
| `member` | `{"node"}` | `{"node", "email", "machine"}` |
| `peers` | none | `{"peers": [{"name", "url", "description", "node"}]}` |

- **`describe`**: `name` is lower-case letters, digits and dashes. `hosts` are the hosts of the URLs the provider gives agents, such as `agentmesh.local`. The server sends requests for them through the CONNECT tunnel.
- **`join`**: joins the network at `url`, the value of `--private` or `--public`, with provider settings in its query. `token` is the `--network-token`, empty without one. `state_dir` keeps the agent's identity across restarts. `private` and `public` say which URLs the agent wants. `node` identifies this agent the way `node` identifies callers. `owner` is the verified email the agent signed in with, or empty.
- **`listen`**: starts forwarding connections to the sockets the server named, and serves the CONNECT tunnel on `connect_socket`. An empty socket means the agent doesn't want that URL. `private_note` says where the private URL works, when that isn't obvious.
- **`member`**: returns the machine `node` as the provider would identify it now.
- **`peers`**: lists other agents on the network. The server fetches each one's agent card at its `url` and drops those that serve none.

| Notification | Params |
|---|---|
| `notice` | `{"message"}`: text for the user, such as a sign-in link |
| `health` | `{"state", "problem", "fix"}`: `state` is `""` when the network reaches the agent, `disconnected` or `removed` |

The server strips control characters from text it shows, and shortens it.

### Errors

Errors use the JSON-RPC codes: `-32700` for a line that isn't JSON (with `id` null), `-32601` for an unknown method, `-32602` for wrong params. Provider errors use `-32000`, with `data` saying what the server should do:

| `data.kind` | Meaning |
|---|---|
| `unsupported` | the provider doesn't implement the method |
| `not_member` | `member`: the network has no such machine |
| `token` | `join`: the network refused the token; `data.problem` says why, `data.fix` how to get a new one |
| `removed` | `join`: the network removed the agent; `data.problem` and `data.fix` say what to do |

## Inbound connections

For each connection to the agent, the provider connects to `private_socket` or `public_socket` and writes a [PROXY protocol v2](https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt) header, then the connection's bytes unchanged in both directions. The server terminates HTTP.

The header has the PROXY command (`0x21`), the `UNSPEC` address family (`0x00`), and these TLVs:

| Type | Value | Required |
|---|---|---|
| `0xE0` | the `token` of `listen` | yes |
| `0xE1` | node: the caller's machine on the network | no |
| `0xE2` | email: the verified email of the caller's person | no |
| `0xE3` | machine: the machine's name for people | no |
| `0xE4` | client key: who shares per-caller limits | no |

Each value is printable UTF-8 of at most 512 bytes, and the header is at most 4096 bytes. The server closes connections without the token, after 10 seconds without a header, and on the public socket with any of node, email or machine. Give every connection a client key: the server treats every provider listener as tunneled, and callers without one share the limits of everyone else without one.

## Outbound connections

The provider serves HTTP on `connect_socket`. The server sends `CONNECT <host>:<port> HTTP/1.1` with `Proxy-Authorization: Bearer <connect_token>`. Answer `407` to a wrong token. Otherwise answer `200`, and the tunnel then carries the server's HTTP/1.1 requests for `http://<host>/...`, one of the `hosts` of `describe`. A plain `502` or `503` response tells the server the agent doesn't answer. A provider that forwards the requests unchanged has nothing more to do. One that turns them into another binding answers `GET` on an agent's URL, and on `/.well-known/agent-card.json` under it, with the agent's card.

## Conformance

The conformance suite runs a provider binary and checks this protocol. Run it from a clone of a2a-serve, with the provider's absolute path:

```bash
go test ./netprovider/conformance -provider /path/to/my-provider -url '<network URL>' -token '<token>'
```

Without `-url`, it skips the checks that join a network.
