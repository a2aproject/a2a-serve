# a2a-run egress proxy protocol

This page specifies how `a2a-run --egress-proxy` asks a host's HTTP proxy about each connection of the agent, for authors of hosts in any language.

> [!NOTE]
> The egress proxy is planned for the next release, with `a2a-run`. The protocol may still change.

With `--egress-proxy`, every TCP connection that the agent opens in the Docker sandbox reaches `a2a-run`, which asks the proxy what to do with it. The proxy can refuse it, tunnel it, or ask for its requests. A Go program that embeds the core gets the same behavior from `egress.NewProxyRouter`.

The proxy URL is `http://host:port` or `unix:///path`. It can't carry credentials or a path.

## Deciding a connection

For a connection to `host:port`, `a2a-run` opens a connection to the proxy and sends:

```
CONNECT api.anthropic.com:443 HTTP/1.1
Host: api.anthropic.com:443
A2A-Run-Context: 7f3c…
A2A-Run-Intercept: offered
```

`A2A-Run-Context` is the A2A context ID of the agent process. It is missing for the connections of the process that `a2a-run` starts to check that the agent starts. Header names are case-insensitive, and `a2a-run` may send them in another case, such as `A2a-Run-Context`.

The proxy's answer picks the route:

| Answer | Route |
| --- | --- |
| `204 No Content` with `A2A-Run-Intercept: accept` | Intercept: `a2a-run` terminates the agent's TLS and sends each request to the proxy, as the next section describes. It closes this connection. |
| Another `2xx`, such as `200 Connection established` | Tunnel: the connection carries the agent's bytes to the destination, as with any HTTP proxy. |
| `4xx` or `5xx` | Refuse: the agent's connection fails. The body, on one line, is the reason in the `Proxy-Status` header that the agent sees. |

Any other answer, an answer that doesn't parse, or no answer in time refuses the connection with `502`. [Limits](limits.md#a2a-run) has the timeout and the size limits.

A proxy that ignores the `A2A-Run-*` headers tunnels or refuses, so an off-the-shelf HTTP proxy works without interception.

## Destinations the proxy must refuse

`a2a-run` checks no destination address of a tunnel or of a request it sends to the proxy. The proxy reaches what it connects to, so the agent reaches it too. Refuse loopback, link-local and private destinations that you don't mean to expose, both by address and by a name that resolves to one. Otherwise the agent can reach services on the host, such as the A2A endpoint of `a2a-run` itself on `--listen 127.0.0.1:port`, and on your network. Check the address that you connect to, after resolving the name.

## Intercepted requests

After `accept`, the agent's requests on that connection go to the proxy in absolute form, over a pool of connections to the proxy:

```
POST https://api.anthropic.com/v1/messages HTTP/1.1
Host: api.anthropic.com
A2A-Run-Context: 7f3c…
A2A-Run-Task: 91ad…
Content-Type: application/json
...
```

- `A2A-Run-Task` is the A2A task that the agent process works on, when a turn is in progress.
- `a2a-run` removes the headers that the agent sent whose names start with `A2A-Run-`, in any case and with `_` in place of `-`, so the agent can't speak for another context.
- The proxy answers as any HTTP proxy does. It can forward the request, rewrite it, or answer it itself. `a2a-run` streams the answer to the agent as it arrives, so server-sent events work.
- A request to switch protocols, such as to a WebSocket, gets `501`.

Connections to port 80 skip the CONNECT step. Their plain HTTP requests go to the proxy in absolute form, such as `GET http://example.com/`.

The agent trusts a CA that `a2a-run` makes at start and keeps in memory. It can sign any host name, since the proxy decides which hosts to intercept as connections open.

## Model credentials

The agent gets placeholder API keys in `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY` and `OPENROUTER_API_KEY`, so that it starts without real ones. Each placeholder starts with the provider's usual key prefix and ends with `a2a-serve-placeholder`, such as `sk-ant-api03-a2a-serve-placeholder`. The proxy replaces the placeholder in intercepted requests with the real key before it forwards them. A variable that `--env` or `--secret` sets keeps its value.

## Trying it

The reference host in [`internal/a2arun/refhost`](https://github.com/a2aproject/a2a-serve/tree/main/internal/a2arun/refhost) serves an egress proxy that intercepts every connection and forwards its requests. `--model-key` gives it the real key of a provider from a variable of your environment:

```console
$ go run ./internal/a2arun/refhost/cmd/refhost --listen unix:///tmp/host.sock \
    --model-key gemini=GEMINI_API_KEY
unix:///tmp/host.sock
```

Pass `unix:///tmp/host.sock` to `--egress-proxy`. The proxy puts the key in place of the agent's placeholder in its requests to the provider's model endpoints.

## Conformance

The conformance suite checks a proxy in any language. From a checkout of a2a-serve, name a host that your proxy intercepts and one that it refuses:

```console
$ go test ./internal/a2arun/conformance -egress-proxy unix:///run/host/egress.sock \
    -intercept api.anthropic.com:443 -refuse blocked.example:443
```

The suite also checks that the proxy refuses this host's loopback. Add `-allow-loopback` for a proxy that means to reach it, and `-forward` as well when it forwards plain HTTP requests to `127.0.0.1`, where the suite serves an upstream that streams events. Without `-egress-proxy`, the suite tests the reference host in `internal/a2arun/refhost`.
