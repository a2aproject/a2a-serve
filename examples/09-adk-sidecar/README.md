# Go ADK Host with `a2a-serve-sidecar`

This example embeds opencode in a Go ADK host using `a2a-serve-sidecar`, bridging ADK tools, model and tool guardrail plugins, and per-session workspaces.

## What is included

- **`A2AServeBackend` (`backend.go`):** starts and reuses `a2a-serve-sidecar` subprocesses and serves their shared control endpoints (`/mcp`, `/gemini/...`, `/anthropic/...`, `/workspace/...`), routing every control request back to the active ADK `InvocationContext` registered under the A2A `context_id`.
- **`SandboxAgent` (`sandbox_agent.go`):** configures the agent profile (`opencode` or `claude`) and ADK `model.LLM` (or model name string), reuses ADK's `remoteagent/v2` (`remoteagent.NewA2A` with session ID forwarded as `ContextID`, Bearer token auth, and `input-required` task resumption) to talk to `a2a-serve-sidecar` over A2A, and registers each run's invocation with `A2AServeBackend`.
- **`SessionWorkspace` (`workspace.go`):** wraps `github.com/a2aproject/a2a-serve/core/fsys` (`fsys.Memory` and `fsys.Handler`) to serve one seeded workspace tree per ADK session and sync workspace files to ADK's `ArtifactService`.
- **ADK `plugin.Plugin` and `GuardrailPlugin` (`guardrail.go`):** wires `BeforeModelCallback`, `AfterModelCallback`, `BeforeToolCallback`, and `AfterToolCallback` on the sandboxed agent's model and tool calls.
- **Official Go MCP SDK (`backend.go`, `tools.go`):** serves `/mcp` via `github.com/modelcontextprotocol/go-sdk/mcp` (`mcp.NewServer` and `mcp.NewStreamableHTTPHandler`), registering ADK tools with `mcp.AddTool` and registering `hooks/beforeTool` and `hooks/afterTool` with `mcp.AddReceivingCustomMethod`.

### Model callbacks

`--egress-middleware` sends Gemini calls (`generativelanguage.googleapis.com`) to `/gemini` and Anthropic calls (`api.anthropic.com`) to `/anthropic`, where `model_adapter.go` translates them to and from ADK's `model.LLMRequest` and `model.LLMResponse`:

- `BeforeModelCallback` sees the whole request: contents with function calls and responses, the system instruction, tools, and generation settings. Changes made by a callback go upstream; an unchanged request goes out as sent. A callback that returns a `*model.LLMResponse` answers the agent in the provider's wire format without calling upstream.
- `AfterModelCallback` sees the whole answer, and a `*model.LLMResponse` that a callback returns replaces it.
- Plugin callbacks run first, then `SandboxAgent`'s own callbacks, and the first one that returns a response wins. A model call that the backend cannot parse is refused.
- When `SandboxAgent` is given an ADK `model.LLM` and the backend has no upstream API key for that provider, the backend invokes `model.LLM.GenerateContent` directly and renders its response back to the agent.

### Sessions and authentication

- `A2AServeBackend` routes each control request by its `A2A-Sidecar-Context` header, which `SandboxAgent` sets to the ADK session ID. A request for a context with no running invocation gets an error: a tool call returns `isError`, and a model call gets `403`.
- `SessionWorkspace` keeps one tree per ADK session, seeded from `workspace/`. Parallel sessions stay isolated. It keeps the trees of the 256 most recently used sessions (`maxSessions`). Call `SessionWorkspace.Forget(sessionID)` to drop a session that ended.
- The backend writes a run-scoped token to `a2a-serve-sidecar`'s stdin and answers control requests without it in `A2A-Sidecar-Token` with `401`. `SandboxAgent` sends `a2a-serve-sidecar`'s own token from its ready line as `Authorization: Bearer` on every A2A request via `auth.StaticToken`.

## 1. Quick start with `demo.sh`

[`demo.sh`](demo.sh) builds `a2a-serve-sidecar` with `CGO_ENABLED=0` and launches the ADK Web UI automatically:

```bash
cd examples/09-adk-sidecar
GEMINI_API_KEY="$GEMINI_API_KEY" ./demo.sh
```

Open `http://localhost:8080` and select `coder`. Pass `--port 8090` to use another port. Try prompts such as:

- *"Inspect sample.py in the workspace and explain what it does."*
- *"Check the host deployment policy for the payments service."*
- *"Deploy the payments service to version v2.0."* (pauses in `input-required` for your approval — reply `once` to approve or `reject` to deny)
- *"Run `rm -rf /tmp/test`."* (blocked by `GuardrailPlugin` → `hooks/beforeTool` returns `deny`)

## 2. Or build and run manually

Build `a2a-serve-sidecar` with `CGO_ENABLED=0` (required for the Linux Docker/bwrap egress boundary):

```bash
cd "$(git rev-parse --show-toplevel)"
CGO_ENABLED=0 go build -o examples/09-adk-sidecar/bin/a2a-serve-sidecar ./cmd/a2a-serve-sidecar
```

Run the ADK Web UI:

```bash
cd examples/09-adk-sidecar
GEMINI_API_KEY="$GEMINI_API_KEY" go run ./cmd/demo web api webui
```

## 3. Or run from the CLI

One-shot prompt with audit summary:

```bash
cd examples/09-adk-sidecar
GEMINI_API_KEY="$GEMINI_API_KEY" go run ./cmd/demo -prompt "Read sample.py and check the deployment policy for payments"
```

Interactive console:

```bash
cd examples/09-adk-sidecar
GEMINI_API_KEY="$GEMINI_API_KEY" go run ./cmd/demo console
```

### Configuration environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `GEMINI_API_KEY` | — | Real Gemini key held only in `A2AServeBackend` (injected by `/gemini` middleware) |
| `ANTHROPIC_API_KEY` | — | Real Anthropic key held only in `A2AServeBackend` (injected by `/anthropic` middleware) |
| `SIDECAR_AGENT` | `opencode` | Agent profile passed to `--agent` (`opencode` or `claude`) |
| `SIDECAR_SANDBOX` | `docker` | `docker`, `gvisor`, `bwrap`, or `local` |
| `ADK_MODEL` | `gemini-3.5-flash-lite` | Model configured on `SandboxAgent` |
| `SIDECAR_EGRESS_ALLOW` | — | Comma-separated extra hosts allowed through `--egress-allow` |
| `SIDECAR_BIN` | `examples/09-adk-sidecar/bin/a2a-serve-sidecar` or on `PATH` | Path to `a2a-serve-sidecar` binary |

## 4. Run tests

```bash
cd examples/09-adk-sidecar
go test -race -v ./...
```

## Next steps

- [a2a-serve-sidecar](../../docs/reference/a2a-serve-sidecar.md) lists every flag, and the ready line, workspace, and contact behavior.
- [Tools endpoint](../../docs/reference/a2a-serve-sidecar-tools.md) specifies the MCP tools endpoint and hook methods.
- [Workspace protocol](../../docs/reference/workspace-protocol.md) specifies the HTTP file API used by `SessionWorkspace`.
