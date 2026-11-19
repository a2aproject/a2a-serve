# a2a-run

`a2a-run` serves a coding agent in a sandbox over A2A to a host process, such as an agent framework, an orchestrator or an eval harness, which starts it and talks to it.

> [!NOTE]
> `a2a-run` is new and planned for the next release. Its flags and its ready line may still change.

`a2a-run` takes everything from flags. It reads no config file and keeps no database. It is an [a2a CLI command plugin](a2a-cli.md#command-plugin), so it also runs as `a2a run`.

## Example

Start opencode in Docker on a project directory, with the OpenRouter key from your environment:

```console
$ a2a-run --agent opencode --workspace dir:$PWD \
    --inject-model-key openrouter=OPENROUTER_API_KEY \
    --listen unix:///tmp/a2a-run.sock
{"ready":true,"a2a":"unix:///tmp/a2a-run.sock","sandbox":"docker (rootless)","workspace":"dir","workspace_sync":"turn","egress":"policy"}
```

Send it a message with the `a2a` CLI:

```console
$ a2a send --unix-socket /tmp/a2a-run.sock -a http://a2a/ "List the files of the workspace"
```

The agent sees a placeholder in `OPENROUTER_API_KEY`, and `a2a-run` adds the real key to the agent's requests to OpenRouter. The task that the agent completes carries the files it changed, and the agent's changes reach `$PWD` when its turn ends. Stop `a2a-run` with SIGINT or SIGTERM.

## Ready line

Once the agent is up, `a2a-run` prints one JSON line on stdout. Logs go to stderr.

| Field | Value |
| --- | --- |
| `ready` | `true` once the agent is up, `false` when it failed to start |
| `a2a` | The A2A endpoint: `unix:///path`, or `http://host:port` with the port it picked for port 0 |
| `sandbox` | `docker`, `docker (rootless)` or `local` |
| `workspace` | `dir` or `memory` |
| `workspace_sync` | `turn` or `live` |
| `egress` | What decides the agent's connections, with `--sandbox docker`: `policy` for `--egress` and `--egress-allow`, `proxy` for `--egress-proxy`, or `none` when Docker here can't route them through `a2a-run`, and the agent uses Docker's network. The log on stderr says why. |
| `error` | Why it failed to start, with `ready` false |

When it fails to start, it prints `{"ready":false,"error":"..."}` and exits with code 2 for invalid flags, or 1 for other errors.

## Egress proxy

With `--egress-proxy`, an HTTP proxy of the host decides every connection that the agent opens. For each connection, the proxy refuses it, tunnels it, or asks for its requests, such as the agent's model calls, and answers them itself or forwards them:

```console
$ a2a-run --agent claude --workspace dir:$PWD \
    --egress-proxy unix:///run/host/egress.sock \
    --listen unix:///tmp/a2a-run.sock
{"ready":true,"a2a":"unix:///tmp/a2a-run.sock","sandbox":"docker (rootless)","workspace":"dir","egress":"proxy"}
```

The agent gets placeholder model keys, and the proxy adds the real ones. `--egress-proxy` needs `--sandbox docker`, and replaces `--egress`, `--egress-allow` and `--inject-model-key`. [a2a-run egress proxy protocol](a2a-run-egress-proxy.md) specifies what the proxy gets and answers.

## A2A endpoint

The endpoint serves A2A over JSON-RPC at `/` and over REST, and the agent card at `/.well-known/agent-card.json`. It doesn't authenticate callers, so it listens only on a Unix socket or a loopback address.

Each A2A context runs in an agent process of its own. A process stops when it is idle, and `--max-processes` bounds how many run at once. `a2a-run` keeps tasks in memory and forgets a context some time after its tasks ended. See [limits](limits.md#a2a-run).

When the agent asks before a tool call, the task waits in `input-required`. Answer it on the same task, as [Approval Messages](approval-messages.md) describes.

## Host tools

`--tools` gives every agent session the host's MCP server, and lets the host decide tool calls:

```console
$ a2a-run --agent opencode --workspace dir:$PWD \
    --inject-model-key openrouter=OPENROUTER_API_KEY \
    --tools unix:///run/host/tools.sock --listen unix:///tmp/a2a-run.sock
```

The agent sees the host's tools as those of the MCP server `host`. When the host advertises hooks, `a2a-run` asks it about each tool call the agent asks permission for, and tells it the result of each tool call. [Tools endpoint](a2a-run-tools.md) specifies the protocol.

## Stopping

On SIGINT or SIGTERM, `a2a-run` fails the tasks in progress, stops the agent processes, and removes the Unix socket and its temporary directories.

## Workspace

`--workspace` gives the agent its files:

- `dir:<path>` is a directory on this machine.
- `memory` starts empty and is held by `a2a-run`. It is lost at exit, so take the files from the tasks.

`--workspace-sync` decides how the agent changes them:

- `turn`, the default, gives each A2A context a private copy of the workspace. Before each turn, the copy gets the changes of the workspace. After the turn, the agent's changes go back to the workspace, and the task gets them as an artifact. Other contexts see the changes from their next turn.
- `live` lets every context edit a `dir:` workspace in place. A `memory` workspace with `live` sync is planned.

With `turn` sync, the agent works in a directory under the state directory, which `--state-dir` sets. A `dir:` workspace larger than `turn` sync copies fails at start with a hint to use `live`. A workspace that grows past the [limits](limits.md#a2a-run) fails the turn with a status message that says to remove files. The next turns still run, so the agent can remove them.

Changes that the agent makes after its turn ended, such as by a process it left running, reach the workspace when its agent process stops. No task lists them, and `a2a-run` logs their paths on stderr.

### Changes artifact

After a turn with `turn` sync that changed files, the task gets an artifact named `a2a-run/changes` before its final status. It lists the changes of that turn only:

- The first part is a data part:

  ```json
  {
    "removed": ["old.py"],
    "symlinks": [{"path": "run", "target": "bin/run.sh"}],
    "omitted": [{"path": "data.bin", "size": 5242880, "reason": "file-too-large"}],
    "unlisted": 12
  }
  ```

  `removed` lists the paths the agent removed, and is always there. The other fields appear only when they have entries. `omitted` lists the changed files whose content the artifact leaves out, with the reason: `file-too-large`, `total-too-large`, `changed-since-sync` when the workspace changed the file again after the sync, or `unreadable` when the file was gone or unreadable after the sync. `unlisted` counts the changes past the number of paths the artifact lists.
- Each further part is a file part with the content of a file that the agent created or changed. Its `filename` is the path in the workspace, and its metadata `mode` has the permission bits, such as `0755`.

Paths are relative to the workspace root and use `/`. New empty directories aren't listed. A turn that changed nothing gets no artifact. See [limits](limits.md#a2a-run) for the sizes.

A completed or failed task sends the artifact before its final status, so a stream shows it. A task whose final status comes before the sync, such as a canceled one, or one that failed for a question with no answer or because `a2a-run` stopped, gets the artifact added afterwards. Read it with `GetTask`.

### Conflicts

When the workspace changed a file during a turn that the agent also changed, the workspace keeps its own version. The agent's version is dropped, and the agent's copy gets the workspace's version. The agent's other changes go back as usual.

The task then fails, even when the agent completed it. Its status message says which files conflicted, and the message's metadata key `a2a-serve/conflicts` lists their paths. The status of such a task:

```json
{"state": "TASK_STATE_FAILED", "message": {"metadata": {"a2a-serve/conflicts": ["main.go"]}, "parts": [{"text": "The workspace changed these files while the agent worked on them, ..."}]}}
```

The key is the same for every embedder of the core, so a host reads one key whether it runs `a2a-run` or embeds the core in Go. A canceled or failed task keeps its state, and has the key when its final status comes after the sync.

## Flags

<!-- generated:a2a-run-flags -->
| Flag | Default | Description |
|---|---|---|
| `--agent name` |  | Agent to serve, a name: claude or opencode |
| `--command words` |  | Command line of the agent, as shell words: replaces the command of --agent, or without --agent starts any agent that speaks ACP on stdio |
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--image string` |  | Container image of the agent (default: the image of --agent); only applies to --sandbox docker |
| `--sandbox string` | `docker` | Where the agent runs: docker, or local for this machine without isolation |
| `--egress mode` |  | Where the Docker sandbox may connect, a mode: internet (the public internet and the --egress-allow hosts) or allowlist (only the --egress-allow hosts and the hosts of --inject-model-key) (default internet) |
| `--egress-allow rule` |  | Host the Docker sandbox may also reach, private ones included: a rule such as nas.lan, \*.example.com, 192.168.1.20 or 192.168.1.0/24, with an optional :port |
| `--egress-proxy URL` |  | HTTP proxy that decides every connection of the Docker sandbox and may intercept its requests, such as its model calls, a URL: http://host:port or unix:///path; the agent gets placeholder model keys, and the proxy adds the real ones. Replaces --egress, --egress-allow and --inject-model-key |
| `--env NAME=value` |  | Variable for the agent: NAME=value, such as DEBUG=1 |
| `--secret NAME` |  | Variable of your environment to pass to the agent, such as an API key: a NAME; the agent can read it |
| `--inject-model-key provider=NAME` |  | API key that a2a-run adds to the Docker sandbox's requests to a model provider; the agent gets a placeholder in the variable: provider=NAME, with provider one of anthropic, gemini, openai, openrouter and NAME a variable of your environment, such as anthropic=ANTHROPIC_API_KEY |
| `--workspace dir:path` |  | Files of the agent: dir:path for a directory, or memory for files that a2a-run keeps in memory and drops at exit |
| `--workspace-sync string` | `turn` | How the agent changes the workspace: turn gives each A2A context a private copy, written back after each of its turns, and returns the changed files with the task; live lets the agent edit a dir: workspace in place |
| `--tools url` |  | Tools endpoint of the host, an MCP server over streamable HTTP: a url, http://, https:// or unix:///path; every agent session gets it as the MCP server host, and a2a-run calls the hooks it advertises |
| `--listen string` | `127.0.0.1:0` | Address of the A2A endpoint: unix:///path, or host:port with a loopback host; port 0 picks a free port |
| `--max-processes int` | `4` | Agent processes that run at once, one per A2A context; an idle process stops to make room. One more process runs for the life of a2a-run, started to check that the agent starts |
| `--home directory` |  | Agent home directory, which every agent process shares (default: a temporary directory, removed at exit) |
| `--state-dir directory` |  | State directory for the files of the agent processes that no process may see, such as their sockets (default: a temporary directory, removed at exit) |
<!-- /generated:a2a-run-flags -->
