# Run a Sandboxed Agent for Your Program

This example runs opencode in Docker with `a2a-run` for a host program, which gives it tools, decides its tool calls, and gets the files it changed back with each task.

## What you'll learn

- How to start `a2a-run` and read its ready line
- How the host serves tools to the agent and decides its tool calls
- How the agent's files come back with each task from a workspace in memory
- How the agent calls another agent that the host offers

## Prerequisites

- The `a2a` CLI (see [Getting Started](../../docs/getting-started.md#install)), Docker, Git and Go.
- An API key for Gemini in `GEMINI_API_KEY`. The agent never sees it.

`a2a-run` has no release yet, so build it from source. The reference host of this repository plays the host program:

```bash
git clone https://github.com/a2aproject/a2a-serve ~/a2a-example8/a2a-serve
cd ~/a2a-example8/a2a-serve
CGO_ENABLED=0 go build -o ~/a2a-example8/bin/a2a-run ./cmd/a2a-run
go build -o ~/a2a-example8/bin/refhost ./internal/a2arun/refhost/cmd/refhost
export PATH="$HOME/a2a-example8/bin:$PATH"
```

## 1. Start the host

In **terminal A**, start the reference host on a Unix socket. It serves one tool, `echo`, and allows every tool call except shell commands that match `rm *`:

```bash
export PATH="$HOME/a2a-example8/bin:$PATH"
cd ~/a2a-example8
refhost --listen unix://$HOME/a2a-example8/host.sock --deny 'rm *'
```

It prints its address and then logs every call it gets.

## 2. Start an agent to call

In **terminal B**, start a second agent that the first one can call. It keeps its files in memory:

```bash
export PATH="$HOME/a2a-example8/bin:$PATH"
a2a-run --agent opencode --model google/gemini-3-flash-preview \
  --inject-model-key gemini=GEMINI_API_KEY \
  --workspace memory --listen unix://$HOME/a2a-example8/planner.sock
```

Once the agent is up, `a2a-run` prints one JSON line:

```
{"ready":true,"a2a":"unix:///home/you/a2a-example8/planner.sock","sandbox":"docker (rootless)","workspace":"memory","workspace_sync":"turn","egress":"policy"}
```

The agent sees a placeholder in `GEMINI_API_KEY`, and `a2a-run` adds the real key to its requests to Gemini.

## 3. Start the agent of the host

In **terminal C**, start the agent that the host works with. `--tools` gives it the host's tools and hooks, and `--contact` lets it call the planner:

```bash
export PATH="$HOME/a2a-example8/bin:$PATH"
a2a-run --agent opencode --model google/gemini-3-flash-preview \
  --inject-model-key gemini=GEMINI_API_KEY --workspace memory \
  --tools unix://$HOME/a2a-example8/host.sock \
  --contact planner=unix://$HOME/a2a-example8/planner.sock \
  --listen unix://$HOME/a2a-example8/coder.sock
```

The ready line lists the contact:

```
{"ready":true,"a2a":"unix:///home/you/a2a-example8/coder.sock",...,"contacts":[{"name":"planner","card":"ok"}]}
```

## 4. Get the files back

In **terminal D**, point the `a2a` CLI at the agent's socket so the commands below stay short, and give the agent time to think:

```bash
cd ~/a2a-example8
export A2ACLI_UNIX_SOCKET=$HOME/a2a-example8/coder.sock A2ACLI_AGENT_CARD=http://a2a/ A2ACLI_TIMEOUT=3m
```

Ask for a file:

```bash
a2a send --save-fileparts out "Create hello.py that prints hello, then run it with python3."
```

The host allows both tool calls, so the task completes without asking you. It carries the artifact `a2a-run/changes` with the file the agent created:

```
Artifacts:
  [a2a-run/changes] {"removed":[]} file: hello.py (text/x-python; charset=utf-8, 15 B)
```

`out/` now has `hello.py`. Terminal A logs a `hooks/beforeTool` and a `hooks/afterTool` call for each tool call.

## 5. Let the host refuse a command

```bash
a2a send "Run the shell command: rm -rf notes"
```

The host denies the command, so it never runs. Terminal A logs `decision=deny`.

## 6. Use a tool of the host

```bash
a2a send "Call the echo tool of the host MCP server with the text hi, and tell me what it returned."
```

The agent answers `echo: hi`, and terminal A logs the `tools/call` request.

## 7. Call another agent

```bash
a2a send "Ask the planner agent what 2+2 is, and tell me its answer."
```

The agent calls the planner with `a2a send`, and `a2a-run` makes the call for it. The agent needs no network access to the planner.

## 8. Stop

Press Ctrl-C in terminals C, B and A. `a2a-run` stops the agent containers and removes its sockets. The files in memory are gone; the tasks carried them.

## Next steps

- [a2a-run](../../docs/reference/a2a-run.md) lists every flag, and the ready line, workspace and contact behavior.
- [Tools endpoint](../../docs/reference/a2a-run-tools.md) specifies the hooks a host serves.
- [Egress proxy](../../docs/reference/a2a-run-egress-proxy.md) lets the host decide every connection of the agent, and answer its model calls.
