# a2a-serve-sidecar tools endpoint

This page specifies the tools endpoint that a host gives `a2a-serve-sidecar` with `--tools`, and the hook methods that `a2a-serve-sidecar` calls on it, for authors of hosts in any language.

> [!NOTE]
> `a2a-serve-sidecar` is new and planned for the next release. This protocol may still change.

## Endpoint

The tools endpoint is an [MCP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#streamable-http) server over streamable HTTP. `--tools` takes one of:

| Value | Endpoint |
| --- | --- |
| `http://host:port/path`, `https://...` | The URL, with its path and query |
| `unix:///path/to.sock` | HTTP on the Unix socket, with the MCP endpoint at `/` |

Every new agent session gets the endpoint as the MCP server `host`. The agent names its tools as it names other MCP tools: `host_echo` in opencode, `mcp__host__echo` in Claude Code.

In a Docker sandbox, the agent reaches the server at `http://tools.sidecar.internal` with the path of the URL. `a2a-serve-sidecar` sends these requests to the endpoint with the endpoint's own host in `Host` and the query of the URL, so the sandbox needs no port of the host machine and never sees the query. It answers requests to other paths with 403, and methods other than `POST`, `GET` and `DELETE` with 405, without sending them. With `--egress-proxy`, these requests don't reach the egress proxy. With `--sandbox local`, the agent connects to the URL itself, so `--tools` must be an `http://` or `https://` URL there.

## Origin

The agent and `a2a-serve-sidecar` both reach the endpoint, so `a2a-serve-sidecar` marks who sent each request in the `A2A-Sidecar-Origin` header:

| Value | Sender |
| --- | --- |
| `a2a-serve-sidecar` | The session of `a2a-serve-sidecar` itself, which sends the hooks |
| `sandbox` | The agent in a Docker sandbox. `a2a-serve-sidecar` drops the `A2A-Sidecar-` headers that the agent sends, in any spelling, and sets this one. |

Accept `hooks/*` only with `A2A-Sidecar-Origin: a2a-serve-sidecar`, and answer the others with 403, so that the agent can't decide its own tool calls or report made-up results. With `--sandbox local`, the agent connects to the endpoint itself and can send any header, so the header doesn't tell it from `a2a-serve-sidecar`.

## a2a-serve-sidecar's session

At start, `a2a-serve-sidecar` opens an MCP session of its own and sends `initialize` with `protocolVersion` `2025-06-18` and `clientInfo.name` `a2a-serve-sidecar`, then `notifications/initialized`. It doesn't start when the endpoint doesn't answer.

The `initialize` result says which hooks the host answers:

```json
{
  "protocolVersion": "2025-06-18",
  "capabilities": {
    "tools": {},
    "experimental": {"a2a-sidecar/hooks": {"beforeTool": true, "afterTool": true}}
  },
  "serverInfo": {"name": "my-host", "version": "1.0.0"}
}
```

Without `a2a-sidecar/hooks`, `a2a-serve-sidecar` calls no hook. It sends `Mcp-Session-Id` and `Mcp-Protocol-Version` on later requests when the endpoint gave them. When the endpoint answers a request or a notification with 404, `a2a-serve-sidecar` starts one new session, ends the old one, and sends the message again. It ends the session with `DELETE` when it stops.

Answers can be `application/json` or a `text/event-stream` that carries the answer as one of its events. [Limits](limits.md#a2a-serve-sidecar) has the size and time limits.

## hooks/beforeTool

`a2a-serve-sidecar` sends this request when the agent asks permission for a tool call:

```json
{"jsonrpc": "2.0", "id": 7, "method": "hooks/beforeTool", "params": {
  "context": "7f3c...",
  "task": "91ad...",
  "call": {"id": "toolu_01", "kind": "execute", "title": "go test ./...", "command": "go test ./...", "mcp": null}
}}
```

| Field | Value |
| --- | --- |
| `context`, `task` | The A2A context and task of the call |
| `call.id`, `call.title` | The agent's ID and description of the call |
| `call.kind` | The ACP tool kind: `read`, `edit`, `delete`, `move`, `search`, `execute`, `think`, `fetch` or `other` |
| `call.command` | The shell command of an `execute` call, or `null` |
| `call.mcp` | `<server>.<tool>` for a tool of an MCP server, such as `host.echo`, or `null` |

The result decides the call:

```json
{"jsonrpc": "2.0", "id": 7, "result": {"decision": "deny", "message": "run the tests with make test"}}
```

| `decision` | Effect |
| --- | --- |
| `allow` | The agent runs the call once. |
| `deny` | The agent gets a rejected call. ACP has no field for a reason, so the agent doesn't see `message`; `a2a-serve-sidecar` logs it on stderr. |
| `ask` | The A2A task waits in `input-required`, and the A2A caller answers as [Approval Messages](approval-messages.md) describes. |

A hook that fails denies the call: an error, an answer after the timeout, or a result without a valid `decision`.

The hook sees the calls that the agent asks permission for. `a2a-serve-sidecar` configures opencode to ask before every tool but those that only read the workspace, such as `read`, `grep` and `list`, and before every MCP tool. Claude Code asks before edits, web requests, MCP tools and the shell commands that its default mode asks about.

## hooks/afterTool

`a2a-serve-sidecar` sends this notification for each tool call that the agent reports as completed or failed:

```json
{"jsonrpc": "2.0", "method": "hooks/afterTool", "params": {
  "context": "7f3c...",
  "task": "91ad...",
  "call": {"id": "toolu_01", "kind": "execute", "title": "go test ./...", "status": "completed", "output": "ok  ./...", "truncated": false}
}}
```

`output` is the text content that the agent reported for the call, or its raw output when it reported no text. `truncated` is set when `a2a-serve-sidecar` cut it. Answer with `202 Accepted`. `a2a-serve-sidecar` sends the notifications in order, one at a time, and drops them when too many wait.

## Check an endpoint

The conformance suite checks a tools endpoint in any language: `initialize`, `tools/list`, an unknown method, the hooks it advertises, and that it refuses them from the sandbox.

```console
$ go test ./internal/sidecar/conformance -tools unix:///run/host/tools.sock
```

The reference host in [`internal/sidecar/refhost`](https://github.com/a2aproject/a2a-serve/tree/main/internal/sidecar/refhost) serves one tool, `echo`, and the hooks. It also serves an egress proxy on the same socket. Run it to try `--tools` by hand:

```console
$ go run ./internal/sidecar/refhost/cmd/refhost --listen unix:///tmp/host.sock --deny 'rm *'
unix:///tmp/host.sock
```
