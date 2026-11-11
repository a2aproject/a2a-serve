# SLIM network provider

`netprovider-slim` is the a2a-serve network provider that puts an agent on a [SLIM](https://docs.agntcy.org/slim/overview/) node. The agent gets the SLIM name `<org>/<namespace>/<agent>`. Other a2a-serve agents on the node call it at `http://slim.local/<org>/<namespace>/<agent>`, and SLIM A2A clients, such as [slim-a2a-go](https://github.com/agntcy/slim-a2a-go), call it by its SLIM name over SLIMRPC. [Networks](../../docs/guide/networks.md#slim-nodes-experimental) shows how users start an agent on a node.

## Build

The provider links the SLIM native library with cgo. Install the library for the bindings version in `go.mod` once, then build:

```sh
cd providers/slim
go run github.com/agntcy/slim-bindings-go/v3/cmd/slim-bindings-setup
go build -o netprovider-slim .
```

`third_party/slim-a2a-go` is a patched copy of slim-a2a-go that builds against the a2a-go version a2a-serve uses. [PATCHED.md](third_party/slim-a2a-go/PATCHED.md) says what changed.

## Network URL

The `--private` URL is the node's address, `http://` or `https://` for TLS, with settings in the query:

```text
http://127.0.0.1:46357?org=a2a&namespace=demo&peers=reviewer,coder
```

| Setting | Meaning | Default |
| --- | --- | --- |
| `org`, `namespace` | the first two parts of the agent's SLIM name | `a2a`, `default` |
| `peers` | the agents in the namespace that `a2a-serve peers` lists | none |

## Identity

The network token is the SLIM shared secret of the node's agents, at least 32 bytes long. Every agent and client on the node needs the same secret.

A shared secret proves only that a caller holds it. Anyone with the secret can claim any SLIM name, so the server knows a caller by the name it claims. Use shared secrets only on nodes of your own machines. JWT and SPIRE identities are planned.

A wrong secret doesn't fail the join: SLIM checks it when agents call each other, and calls between agents with different secrets time out.

## Calling the agent from a SLIM client

A slim-a2a-go client creates a SLIM app of its own with the same secret and calls the agent by its SLIM name:

```go
app, _ := svc.CreateAppWithSecret(slim_bindings.NewName("a2a", "demo", "client"), secret)
conn, _ := svc.Connect(slim_bindings.NewInsecureClientConfig("http://127.0.0.1:46357"))
_ = app.Subscribe(app.Name(), &conn)
channel := slim_rpc.ChannelNewWithConnection(app, slim_bindings.NewName("a2a", "demo", "coder"), &conn)
t := a2aslimrpc.NewTransport(channel)
res, err := t.SendMessage(ctx, nil, &a2a.SendMessageRequest{Message: msg})
```

`GetExtendedAgentCard` returns the agent's card with a `slimrpc` interface at its SLIM name.

## Tests

The integration tests need a SLIM node at `$SLIM_ENDPOINT` (default `http://127.0.0.1:46357`) that runs without TLS, and skip without one:

```sh
docker run -d --name slimnode -p 127.0.0.1:46357:46357 \
  -v "$PWD/../../examples/07-slim-network/slim-node.yaml:/config.yaml:ro" \
  ghcr.io/agntcy/slim:latest /slim --config /config.yaml
go test -race ./...
```

To run the network provider conformance suite against a built binary:

```sh
go test github.com/a2aproject/a2a-serve/netprovider/conformance \
  -provider "$PWD/netprovider-slim" \
  -url 'http://127.0.0.1:46357?namespace=conformance' \
  -token "$SLIM_SECRET"
```
