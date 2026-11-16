# A coding agent in a sandbox, as an ADK agent

This sample runs `opencode acp` as an ADK agent written in Python. Every ADK session gets its own Docker container, whose `/workspace` lives in memory, and whose network is only what code in [`agent.py`](agent.py) allows. It is the Python counterpart of the [acpagent sample of adk-go](https://github.com/google/adk-go/pull/2021).

- **The workspace lives in memory.** The container mounts a tmpfs at `/workspace`, seeded from a dict in `agent.py`. What the agent writes there never reaches the host's disk. Python reads the files back when the run ends. The rest of the container, such as opencode's own state in its home, is Docker's usual writable layer.
- **Code controls the coding model's traffic.** opencode talks to `http://llm.internal/v1`. The interceptor sends those requests on to Vertex AI's OpenAI-compatible endpoint, with an access token of the host's credentials, and stops them past a budget of calls. The container never holds a credential.
- **ADK tools reach the agent over MCP.** `http://adk-tools.internal/mcp` is answered in process by `AcpAgent.handle_mcp`. `file_ticket` runs in the ADK invocation, and its call, response and state change are events of the session.
- **A guard model decides the rest.** Any other connection, or plain HTTP request, goes to Gemini, which answers allow or deny.

## Requirements

- Linux with Docker and `/dev/net/tun`.
- Application default credentials for a Google Cloud project with Vertex AI: `gcloud auth application-default login`. `GOOGLE_CLOUD_PROJECT` picks another project than the credentials' own.
- The libraries and the package: `clib/build.sh`, then `pip install -e 'python[adk]'`.

## Run

```sh
cd python/samples/adk_sandbox_coder
python main.py
```

| Variable | Default | Meaning |
|---|---|---|
| `ACP_IMAGE` | `ghcr.io/a2aproject/harness-opencode:latest` | The image of the containers |
| `ACP_COMMAND` | `opencode acp` | The agent's command |
| `CODE_MODEL` | `google/gemini-2.5-flash` | The model behind `llm.internal` |
| `GUARD_MODEL` | `gemini-2.5-flash` | The model that decides other traffic |
| `MAX_MODEL_CALLS` | `60` | The budget of model calls per session |
| `SANDBOX_LOG` | unset | `1` prints each decision as it happens |

`adk web` and `adk run` load the agent from `agent.py` as `root_agent` too.

## Example run

Real output, trimmed. The model decides what it does, so your output will differ.

```text
$ python main.py
User -> Run the tests with `python3 -m unittest -q`, fix the bugs in stats.py until they pass, then file one ticket with file_ticket for a missing test you would add. Reply with a one-paragraph summary and the ticket ID.

[agent tool] python3 -m unittest -q (completed)
[agent tool] workspace/stats.py (completed)
[agent tool] workspace/stats.py (completed)
[agent tool] python3 -m unittest -q (completed)
[agent tool] adk_file_ticket (in_progress)
[adk tool call] file_ticket({'body': 'The median function in stats.py needs a test case for an input list with an odd number of elements to ensure correctness.', 'title': 'Add test for median function with odd number of elements'})
[adk tool result] {'id': 'T-1'}
[agent tool] adk_file_ticket (completed)
Agent -> I ran the existing tests, fixed the bugs in `stats.py` related to `mean` and `median` calculations, and re-ran the tests to confirm the fixes. [...] I also filed a ticket (ID: T-1) for a missing test case for the `median` function, specifically for lists with an odd number of elements.

Workspace in memory:
  __pycache__/stats.cpython-313.pyc
  __pycache__/test_stats.cpython-313.pyc
  stats.py
  test_stats.py

Tests in the sandbox: exit 0
Ran 2 tests in 0.000s

OK

Tickets in the session state: [{'id': 'T-1', 'title': 'Add test for median function with odd number of elements', 'body': '...'}]

Interceptor log:
  sandbox ready in 1.5s: docker: using ghcr.io/a2aproject/harness-opencode:latest
  mcp POST initialize -> 200
  mcp POST tools/list -> 200
  model call 1: google/gemini-2.5-flash with 3 messages -> Vertex AI
  [...]
  connect registry.npmjs.org:443 -> deny (guard model)
  model call 6: google/gemini-2.5-flash with 11 messages -> Vertex AI
  mcp POST tools/call -> 200
  model call 7: google/gemini-2.5-flash with 13 messages -> Vertex AI
```

opencode tried to reach the npm registry while it edited, and the guard model refused, which didn't stop the run.

## Notes

- **One container per ADK session.** opencode stops after 10 idle minutes and starts again in the same container on the next message, so the workspace survives. Closing the agent removes the containers and the workspaces.
- **Tool callbacks, plugins and confirmation don't apply** to the tools that the coding agent calls, as in adk-go.
- **HTTPS to other hosts isn't inspected.** The guard model sees only the host and port. List hosts in the sandbox's `inspect_tls` to see their requests too, for clients that trust the sandbox's CA.
