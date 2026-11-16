# C libraries

This directory builds two C libraries from Go packages, for programs in other languages. The Python package in [`python/`](../python) loads both.

- `liba2asandbox.so` runs a command in a Docker container through the [`sandbox`](../sandbox) package. The container can have a file system in memory, and code in the caller can decide each of its connections.
- `liba2aacp.so` runs an agent of the Agent Client Protocol, such as `opencode acp`, as an A2A executor through [a2aacp](https://github.com/a2aproject/a2a-go/tree/main/integrations/executor/a2aacp).

## Build

```sh
clib/build.sh            # into python/src/a2a_serve/_lib
clib/build.sh /some/dir  # anywhere else
```

The script also builds `a2a-sandbox-init`, the launcher that runs as PID 1 of the containers. It is a static binary built without cgo, which the libraries can't be. Pass its path as `launcher` in the spec of a sandbox.

Each library has its own Go runtime, so a process can load both. They share nothing in memory. The sandbox library hands the stdio of a command to the ACP library as a file descriptor.

## Calling convention

Every function returns a JSON object as a `char*`. Free it with `a2a_sandbox_free` or `a2a_acp_free`. A failed call returns `{"error": "..."}`. Byte strings are base64 in JSON.

A library asks its caller questions through a queue. `*_next_event(timeout_ms)` returns the next question as `{"id", "type", "data"}`, or `{}` when none came in time. `*_reply(id, answer_json)` answers it. A question that gets no answer in time fails. Answer questions on a worker thread, since several can wait at once.

## liba2asandbox.so

| Function | Result |
|---|---|
| `a2a_sandbox_open(spec)` | `{"handle", "info"}` |
| `a2a_sandbox_start(h)` | `{"fd"}`: a Unix socket that carries the command's stdio. The caller owns it, and closing it stops the command. |
| `a2a_sandbox_read_file(h, name)` | `{"data"}` |
| `a2a_sandbox_write_file(h, name, b64)` | `{}` |
| `a2a_sandbox_list(h, dir)` | `{"files"}`: the paths of the files under `dir` |
| `a2a_sandbox_exec(h, req)` | `{"stdout", "stderr", "exit_code"}` for `{"argv", "stdin", "timeout_ms"}`. It runs next to the command, with its user, capabilities and network. |
| `a2a_sandbox_stderr(h)` | `{"stderr"}`: the end of the command's stderr |
| `a2a_sandbox_close(h)` | `{}`: removes the containers and the file system in memory |

A spec:

```json
{
  "name": "coder-1",
  "image": "ghcr.io/a2aproject/harness-opencode:latest",
  "command": {"path": "opencode", "args": ["acp"], "dir": "/workspace", "env": ["A=1"]},
  "memory": {"dir": "/workspace", "size": "128m", "files": {"main.py": "cHJpbnQoMSkK"}},
  "network": {"mode": "allowlist", "allow": [], "intercept": true, "inspect_tls": []},
  "launcher": "/path/to/a2a-sandbox-init"
}
```

File paths are relative to the memory directory, and the file calls refuse paths outside it. The file system in memory outlives restarts of the command until the sandbox closes.

Without `network`, the container gets Docker's own network. With it, the container's only network path is the egress boundary, which applies `mode` and the rules of `allow`. With `intercept`, the boundary asks the caller first.

`connect` questions carry `{"sandbox", "host", "port"}`. Answer `{"action": ...}`:

- `allow` allows the connection to the host's public addresses, as the internet mode does. Private addresses, such as those of the LAN or the tailnet, still need a rule of `allow` in the spec.
- `deny` refuses it.
- `policy` leaves it to the rules.
- `inspect` accepts it without dialing the host and asks about each HTTP request on it. Plain HTTP works for any host. TLS works for the hosts of `inspect_tls`, whose certificates the sandbox trusts.

`request` questions carry `{"sandbox", "method", "url", "headers", "body"}`. Answer one of:

```json
{"action": "respond", "status": 200, "headers": {"Content-Type": ["application/json"]}, "body": "e30="}
{"action": "forward", "url": "https://elsewhere/v1/x", "set_headers": {"Authorization": "Bearer ..."}, "remove_headers": ["X-Api-Key"]}
{"action": "deny", "status": 403, "body": "..."}
```

`forward` sends the request, changed, to the host of its URL as an `allow` would, and streams the response back. A connection waits for its answer, so answer `connect` within seconds.

`a2a_sandbox_exec` stops a command that runs out of `timeout_ms` when the sandbox has the boundary, and reports the timeout as an error. Without the boundary, the command runs until it exits or the sandbox closes.

Each container and volume carries the label `a2a.sandboxlib.owner` with the process that made it. The first `a2a_sandbox_open` of a process removes those of processes that are gone, such as after a crash.

## liba2aacp.so

| Function | Result |
|---|---|
| `a2a_acp_new(config)` | `{"handle"}` |
| `a2a_acp_send(h, request)` | `{"stream"}` for an A2A `SendMessageRequest` |
| `a2a_acp_stream_next(stream, timeout_ms)` | `{"event"}` with an A2A `StreamResponse`, `{"done": true}` at the end, or `{}` |
| `a2a_acp_stream_close(stream)` | `{}`. The task goes on. |
| `a2a_acp_cancel(h, task_id)` | `{"task"}` |
| `a2a_acp_close(h)` | `{}`: stops the agents |

A config:

```json
{
  "id": "caller-chosen-name",
  "external": true,
  "per_context": true,
  "idle_timeout_ms": 600000,
  "session": {"cwd": "/workspace", "mcp_servers": [{"type": "http", "name": "adk", "url": "http://adk-tools.internal/mcp", "headers": []}]},
  "allow": ["read", "search"],
  "deny": [],
  "settings": {"model": "provider/model"}
}
```

`agent: {"argv", "dir", "env"}` runs the agent on this host. With `external`, the library asks the caller to start each agent with a `start` question, which carries the config's `id` and the agent's `key`, the A2A context with `per_context`. Answer `{"fd": N}` with the agent's stdio, such as the `fd` of `a2a_sandbox_start`, and the library takes it over.

`allow` and `deny` answer the agent's permission requests by ACP tool kind. Other requests leave the task in input-required, and the next message to the task answers them.
