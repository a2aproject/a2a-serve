# Python bindings

The `a2a_serve` package wraps the [C libraries](../clib) of the a2a-serve sandbox and of a2aacp, and adds an ADK agent for coding agents that speak the Agent Client Protocol.

- `a2a_serve.Sandbox` runs a command in a Docker container. Its working directory can live in memory, and an interceptor in Python can decide each of its connections.
- `a2a_serve.Executor` runs an ACP agent, such as `opencode acp`, and returns the A2A events of each message.
- `a2a_serve.adk.AcpAgent` is an ADK agent backed by an `Executor`. With `sandbox`, each ADK session gets its own sandbox with a workspace in memory; with `model`, an ADK model answers the coding agent's model calls, with the model callbacks; with `tools`, it calls ADK tools over MCP.

## Setup

```sh
clib/build.sh
pip install -e 'python[adk]'
```

The package needs Linux and, for sandboxes, Docker.

## Run a command in a sandbox

```python
from a2a_serve import Sandbox, respond

class Interceptor:
    def connect(self, host, port):
        return "inspect" if host == "api.internal" else "deny"

    def request(self, req):
        return respond(200, '{"answer": 42}', {"Content-Type": "application/json"})

with Sandbox("demo-1", "python:3.13-slim", ["sleep", "infinity"],
             memory_dir="/workspace", files={"main.py": "print('hi')"},
             interceptor=Interceptor()) as sb:
    sock = sb.start()
    print(sb.exec(["python3", "main.py"]).stdout)
    print(sb.exec(["python3", "-c", "import urllib.request; print(urllib.request.urlopen('http://api.internal/').read())"]).stdout)
```

`files`, `read_file`, `write_file` and `list_files` work on the file system in memory, which lives until the sandbox closes.

## Run an ACP agent

```python
from a2a_serve import Executor

ex = Executor(argv=["opencode", "acp"], allow=["read", "search"])
for event in ex.stream("Summarize README.md"):
    print(event)
ex.close()
```

With `start` instead of `argv`, and `per_context=True`, the executor asks for an agent per A2A context. Return `sandbox.start_fd()` to run each one in its own sandbox.

## Sample

[`samples/adk_sandbox_coder`](samples/adk_sandbox_coder) runs opencode as an ADK agent in a sandbox with a workspace in memory. Code decides its traffic: the model's requests go to Vertex AI with the host's credentials, ADK tools answer at a virtual host, and a guard model decides the rest.

## Tests

```sh
CGO_ENABLED=0 go build -o /tmp/fakeacp ./internal/agentsrv/testdata/fakeacp
cd python && FAKEACP=/tmp/fakeacp python -m unittest discover -s tests
```

Set `FAKEACP_IMAGE` to an image with fakeacp at `/usr/local/bin/fakeacp` to also test the Docker sandbox.
