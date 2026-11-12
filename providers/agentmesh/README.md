# netprovider-agentmesh

`netprovider-agentmesh` is the a2a-serve network provider of [Agent Mesh](https://github.com/google/agentmesh) relays. It joins a relay with meshnet and gives the agent a private URL on the relay, a public URL on the relay's ingress, verified caller identity and discovery of the other agents. It speaks the network provider protocol of the [`netprovider`](../../netprovider) package, and the server runs it as a subprocess with `netprovider-agentmesh serve`.

It joins relays with Agent Mesh v0.2.0. Relays should run the same version.

## How a2a-serve finds it

`agentmesh` is the default provider of `https://` and `http://` network URLs. The server looks for the `netprovider-agentmesh` binary in this order:

1. Next to the `a2a-serve` binary, where release archives put it.
2. In `../libexec/a2a-serve` relative to the `a2a-serve` binary, where Homebrew keeps it.
3. In the a2a-serve data directory, where the server installs the pinned release.

The provider keeps the agent's identity in the agent home directory, in `.a2a/state/agentmesh`, so the agent keeps its URL across restarts.

## Build

The provider builds without cgo:

```sh
cd providers/agentmesh
CGO_ENABLED=0 go build -o "$(dirname "$(command -v a2a-serve)")/netprovider-agentmesh" .
```

## Test

```sh
cd providers/agentmesh
go test ./...
```

The tests run real Agent Mesh relays. They download the pinned `agentmesh-one` into the user cache directory on the first run, or use the binary that `A2A_SERVE_AGENTMESH_ONE` names. Run `go test -short ./...` to skip them.

`TestRelayConformance` runs the protocol conformance suite of [`netprovider/conformance`](../../netprovider/conformance) against the built binary and a local relay. To run the suite against another relay, build the binary and pass an invite:

```sh
go test github.com/a2aproject/a2a-serve/netprovider/conformance \
  -provider "$PWD/netprovider-agentmesh" -url https://relay.example.com -token "<invite>"
```

Create the invite on the relay with `a2a-serve relay invite conformance`.
