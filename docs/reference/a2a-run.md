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
{"ready":true,"a2a":"unix:///tmp/a2a-run.sock","sandbox":"docker (rootless)","workspace":"dir","egress":"policy"}
```

Send it a message with the `a2a` CLI:

```console
$ a2a send --unix-socket /tmp/a2a-run.sock -a http://a2a/ "List the files of the workspace"
```

The agent sees a placeholder in `OPENROUTER_API_KEY`, and `a2a-run` adds the real key to the agent's requests to OpenRouter. Stop `a2a-run` with SIGINT or SIGTERM.

## Ready line

Once the agent is up, `a2a-run` prints one JSON line on stdout. Logs go to stderr.

| Field | Value |
| --- | --- |
| `ready` | `true` once the agent is up, `false` when it failed to start |
| `a2a` | The A2A endpoint: `unix:///path`, or `http://host:port` with the port it picked for port 0 |
| `sandbox` | `docker`, `docker (rootless)` or `local` |
| `workspace` | `dir` |
| `egress` | With `--sandbox docker`: `policy` when `--egress` and `--egress-allow` decide the agent's connections, or `proxy` with `--egress-proxy` |
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

On SIGINT or SIGTERM, `a2a-run` fails the tasks in progress, stops the agent processes, and removes the Unix socket and its temporary directories.

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
| `--workspace dir:path` |  | Workspace that the agent processes share: dir:path, a directory |
| `--listen string` | `127.0.0.1:0` | Address of the A2A endpoint: unix:///path, or host:port with a loopback host; port 0 picks a free port |
| `--max-processes int` | `4` | Agent processes that run at once, one per A2A context; an idle process stops to make room. One more process runs for the life of a2a-run, started to check that the agent starts |
| `--home directory` |  | Agent home directory, which every agent process shares (default: a temporary directory, removed at exit) |
| `--state-dir directory` |  | State directory for the files of the agent processes that no process may see, such as their sockets (default: a temporary directory, removed at exit) |
<!-- /generated:a2a-run-flags -->
