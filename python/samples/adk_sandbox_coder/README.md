# A coding agent in a sandbox, as an ADK agent

This sample runs `opencode acp` as an ADK agent written in Python, with an ADK model as opencode's model. Every ADK session gets its own Docker container, whose `/workspace` lives in memory, and whose network is only what [`agent.py`](agent.py) allows. It is the Python counterpart of the [acpagent sample of adk-go](https://github.com/google/adk-go/pull/2021).

The whole agent is one `AcpAgent`:

```python
root_agent = AcpAgent(
    name="sandboxed_coder",
    model="gemini-2.5-flash",
    before_model_callback=model_budget,
    sandbox=SandboxConfig(image=IMAGE, command=["opencode", "acp"], files=WORKSPACE, env=OPENCODE_ENV, egress=GuardEgress()),
    allow_tools=["*"],
    tools=[FunctionTool(file_ticket)],
)
```

- **The workspace lives in memory.** The container mounts a tmpfs at `/workspace`, seeded from a dict in `agent.py`. What the agent writes there never reaches the host's disk. Python reads the files back when the run ends. The rest of the container, such as opencode's own state in its home, is Docker's usual writable layer.
- **opencode's model is the agent's ADK model.** opencode's only provider is `http://llm.internal/v1`. AcpAgent answers its requests by turning each into an ADK `LlmRequest` for `model`, here Gemini on Vertex AI with the host's credentials. The container never holds a credential. `before_model_callback` counts the calls in the session state and refuses them past a budget, as it would for an `LlmAgent`.
- **ADK tools reach the agent over MCP.** `http://adk-tools.internal/mcp` is answered in process by `AcpAgent.handle_mcp`. `file_ticket` runs in the ADK invocation, and its call, response and state change are events of the session.
- **A guard model decides the rest.** `SandboxConfig.egress` gets every other connection, or plain HTTP request, and asks Gemini, which answers allow or deny.

## Goal

Show that a coding agent you don't control can work as an ADK agent, with ADK deciding what it touches: the files it starts with and leaves behind, the model it thinks with, the tools it may call, and every connection it opens.

## How it works

```mermaid
sequenceDiagram
    actor User
    participant ADK as ADK runner
    participant Agent as AcpAgent
    participant Boundary as sandbox boundary
    participant Coder as opencode (in the sandbox)

    Note over Agent,Coder: first message of an ADK session
    User->>ADK: prompt
    ADK->>Agent: run invocation
    Agent->>Boundary: start opencode, workspace in memory
    Agent->>Coder: session/new with the MCP server adk, session/prompt
    Coder->>Boundary: POST http://llm.internal/v1/chat/completions
    Boundary->>Agent: request (interceptor)
    Agent->>Agent: before_model_callback, then the ADK model
    Agent-->>Boundary: chat completion
    Coder->>Boundary: MCP tools/call file_ticket at adk-tools.internal
    Boundary->>Agent: request (interceptor)
    Agent->>ADK: function call event
    Agent->>Agent: run file_ticket with the invocation's tool context
    Agent->>ADK: function response event with the state delta
    Agent-->>Coder: tool result
    Coder->>Boundary: CONNECT registry.npmjs.org:443
    Boundary->>Agent: connect (interceptor)
    Agent-->>Boundary: deny (guard model)
    Coder-->>Agent: reply
    Agent-->>User: reply
```

## Compared with adk-go's acpagent

`a2a_serve.adk.AcpAgent` follows the `exp/acpagent` package of adk-go:

| acpagent (Go) | AcpAgent (Python) |
|---|---|
| `Command`, `Dir`, `Env`, or `Start` for an agent that runs elsewhere | `argv`, `cwd` and `env`; `start`, which returns the descriptor of the agent's stdio; or `sandbox`, which runs each session's agent in a sandbox of its own |
| Each ADK session maps to its own ACP session; the agent gets only the messages it hasn't seen | The same: an ACP session per ADK session, and what the agent wasn't sent, with the messages of others marked with their author |
| `AllowTools`, `DenyTools` answer permission requests by ACP tool kind | `allow_tools`, `deny_tools` |
| Other questions end the invocation, and the user's next message answers them | The same; the question is the invocation's last event, with `acp_input_required` in its metadata |
| `Tools` served over MCP on a loopback port, also to a local `Command`; calls and responses are events of the invocation | `tools` served over MCP at `mcp_url`, which the sandbox's interceptor answers in process, so nothing listens on the host; calls and responses are events of the invocation. A local `argv` gets no tools, and AcpAgent refuses that config |
| An abandoned run cancels the agent's work | The same, also when the run ends before its first event |
| One agent process for every session | One per ADK session, so that each session gets its own sandbox; it stops when idle and starts again on the next message |
| The agent uses the model of its own configuration | `model` answers the agent's model calls with an ADK model, with the model callbacks, once the agent's configuration names `model_url` as its provider, as `agent.py` does for opencode |

The tests in [`python/tests/test_adk_agent.py`](../../tests/test_adk_agent.py) follow those of acpagent: conversation, permissions, answers in a later invocation, tools, tools after an abandoned run, tools per session, and the config checks.

## Requirements

- Linux with Docker and `/dev/net/tun`.
- Application default credentials for a Google Cloud project with Vertex AI: `gcloud auth application-default login`. `GOOGLE_CLOUD_PROJECT` picks another project than the credentials' own.
- The libraries and the package: `clib/build.sh`, then `pip install -e 'python[adk]'`.

## Run

```sh
cd python/samples/adk_sandbox_coder
python main.py            # one turn with the default prompt
python main.py console    # a conversation, one line per message
```

opencode asks before it edits a file or runs a command, and `allow_tools` answers. By default it allows everything, since the sandbox contains what opencode does. With `ALLOW_TOOLS=read,search,think,other`, the other requests reach you: the agent's turn ends with the question, and your next message answers it with `once`, `always` or `reject`.

| Variable | Default | Meaning |
|---|---|---|
| `ACP_IMAGE` | `ghcr.io/a2aproject/harness-opencode:latest` | The image of the containers |
| `ACP_COMMAND` | `opencode acp` | The agent's command |
| `MODEL` | `gemini-2.5-flash` | The ADK model behind `llm.internal` |
| `GUARD_MODEL` | `gemini-2.5-flash` | The model that decides other traffic |
| `MAX_MODEL_CALLS` | `60` | The budget of model calls per session, which `before_model_callback` keeps |
| `MAX_GUARD_CALLS` | `40` | The budget of guard-model questions per session; past it, other traffic is denied |
| `SANDBOX_IDLE` | `1800` | Seconds without traffic or a turn in progress after which a session's sandbox and workspace are removed. It is at least a minute longer than the 10 minutes after which the agent stops. |
| `ALLOW_TOOLS` | `*` | The ACP tool kinds opencode may use without asking you |
| `SANDBOX_LOG` | unset | `1` prints each decision as it happens |

`adk web` and `adk run` load the agent from `agent.py` as `root_agent` too.

## Example run

Real output of one run. The model decides what it does, so your output will differ.

```text
$ python main.py
User -> Run the tests with `python3 -m unittest -q`, fix the bugs in stats.py until they pass, then file one ticket with file_ticket for a missing test you would add. Reply with a one-paragraph summary and the ticket ID.

[agent tool] python3 -m unittest -q (in_progress)
[agent tool] python3 -m unittest -q (completed)
[agent tool] read (in_progress)
[agent tool] workspace/stats.py (completed)
[agent tool] edit (in_progress)
[agent tool] workspace/stats.py (completed)
[agent tool] python3 -m unittest -q (in_progress)
[agent tool] python3 -m unittest -q (completed)
[agent tool] edit (in_progress)
[agent tool] workspace/stats.py (completed)
[agent tool] python3 -m unittest -q (in_progress)
[agent tool] python3 -m unittest -q (completed)
[agent tool] adk_file_ticket (in_progress)
[adk tool call] file_ticket({'body': 'The test_stats.py file is missing a test case for the median function with an odd number of elements. Please add `test_median_odd` to ensure the function works correctly for odd-length lists.', 'title': 'Add test_median_odd to test_stats.py'})
[adk tool result] {'id': 'T-1'}
[agent tool] adk_file_ticket (completed)
Agent -> I identified and fixed two bugs in `stats.py`. [...] I have filed a ticket (ID: T-1) to request the addition of a `test_median_odd` case to `test_stats.py` [...] for the `median` function.


Workspace in memory:
  __pycache__/stats.cpython-313.pyc
  __pycache__/test_stats.cpython-313.pyc
  stats.py
  test_stats.py

stats.py after the run:
def mean(values):
    """Returns the arithmetic mean of values."""
    return sum(values) / len(values)


def median(values):
    """Returns the median of values."""
    s = sorted(values)
    n = len(s)
    if n % 2 == 1:
        return s[n // 2]
    else:
        mid1 = s[n // 2 - 1]
        mid2 = s[n // 2]
        return (mid1 + mid2) / 2

Tests in the sandbox: exit 0
----------------------------------------------------------------------
Ran 2 tests in 0.000s

OK

Tickets in the session state: [{'id': 'T-1', 'title': 'Add test_median_odd to test_stats.py', 'body': '...'}]
Model calls in the session state: 9

Guard log:
  connect registry.npmjs.org:443 -> deny (guard model)
  connect registry.npmjs.org:443 -> deny (guard model)
  connect registry.npmjs.org:443 -> deny (guard model)
  connect registry.npmjs.org:443 -> deny (guard model)
```

The 9 model calls went through ADK's Gemini model, and `model_budget` counted them in the session state. opencode tried to reach the npm registry while it edited, and the guard model refused, which didn't stop the run.

A conversation in the console, where opencode has to ask before it edits (the console doesn't echo piped input, so the answers are added here):

```text
$ printf 'Fix the bug in the mean function in stats.py. Reply with one sentence.\nonce\n' | ALLOW_TOOLS=read,search,think,other python main.py console
User -> Fix the bug in the mean function in stats.py. Reply with one sentence.
[agent tool] glob (in_progress)
[agent tool] workspace (completed)
[agent tool] read (in_progress)
[agent tool] workspace/stats.py (completed)
[agent tool] edit (in_progress)
Agent -> Permission required: /workspace/stats.py.
Reply with one of: once, always, reject.

User -> once
[agent tool] workspace/stats.py (completed)
Agent -> The bug in the `mean` function in `stats.py` has been fixed by removing the `+ 1` from the return statement.
```

## Notes

- **One container per ADK session.** opencode stops after 10 idle minutes and starts again in the same container on the next message, so the workspace survives. After `SANDBOX_IDLE`, the sandbox and its workspace are removed, and the next message starts in a fresh one. Closing the agent removes them all.
- **Tool callbacks, plugins and confirmation don't apply** to the tools that the coding agent calls, as in adk-go. The model callbacks apply to its model calls; plugins and `on_model_error` callbacks don't.
- **Model answers arrive whole.** A model call that opencode streams gets its answer in one piece once the model is done. Errors of the model reach opencode as HTTP errors it retries when they are transient, such as a rate limit. A refusal of `before_model_callback`, such as the budget's, ends opencode's turn.
- **Tool results.** A tool that raises gives opencode an MCP error with `{"error": "<message>"}`, and a result that isn't a dict reaches it as `{"result": ...}`.
- **Questions live in opencode's process.** A question that waits longer than opencode's idle stop, 10 minutes, is lost with the process; the next message starts afresh.
- **Gemini 3 works too.** `MODEL=gemini-3-flash-preview` runs the sample; the thought signatures that Gemini 3 needs back travel in the IDs of the tool calls.
- **HTTPS to other hosts isn't inspected.** The guard model sees only the host and port. List hosts in the sandbox's `inspect_tls` to see their requests too, for clients that trust the sandbox's CA.
