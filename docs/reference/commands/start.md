# a2a-serve start

<!-- Generated from the command definitions; do not edit. -->

Without an argument, start the agent saved in the nearest .a2a directory
(or --dir). On a first run in a terminal, it asks a few questions instead.

With a harness, create the agent in this directory on the first run, or apply
the flags to the existing agent like `a2a-serve config set`, then start it.

echo, proxy and exec run a local test server instead, without an agent
directory.

```
a2a-serve start [<harness>|echo|proxy|exec] [flags]
```

**Examples**

```bash
a2a-serve start opencode --name coder
a2a-serve start
a2a-serve start --detach
a2a-serve start echo --port 9000
```

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--quiet` |  | Print less on the terminal: keep the harness image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |

## a2a-serve start acp

Start any ACP agent (--command) in this directory. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like `a2a-serve config set`.

```
a2a-serve start acp [flags]
```

**Examples**

```bash
a2a-serve start acp --name coder --secret OPENROUTER_API_KEY
a2a-serve start acp --private tailscale --allow-tools read,search --detach
a2a-serve start acp --workspace ~/src/app --mount ~/docs:/docs:ro
```

**General flags**

| Flag | Default | Description |
|---|---|---|
| `--description string` |  | Agent description for its card |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--env NAME=value` |  | Extra variable for the harness: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME` |  | Name of an environment variable to pass to the harness, such as an API key (NAME); its value is read from your environment (or prompted) and stored in .a2a/state/secrets (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the ACP agent, as shell words, such as "gemini --experimental-acp" |
| `--image string` |  | Container image of the harness; only applies to --sandbox docker |
| `--sandbox string` |  | Where the harness runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--identity login=member\|owner` |  | Role of another user's network login: login=member\|owner, such as anna@github=owner (repeatable) |
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, lan, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), ngrok, none, or the URL of an Agent Mesh relay |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools kinds` |  | ACP tool kinds approved without asking: read, edit, delete, move, search, execute, think, fetch, switch_mode, other, or a glob (repeatable; commas also separate values) |
| `--deny-tools kinds` |  | ACP tool kinds denied without asking, like --allow-tools (repeatable; commas also separate values) |
| `--owner-chat channel[:who]` |  | Where an owner answers the approvals of tasks nobody watches: channel[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--dir string` |  | Agent directory (default: the current directory) |
| `--quiet` |  | Print less on the terminal: keep the harness image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

## a2a-serve start claude

Start a claude agent in this directory. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like `a2a-serve config set`.

```
a2a-serve start claude [flags]
```

**Examples**

```bash
a2a-serve start claude --name coder --secret OPENROUTER_API_KEY
a2a-serve start claude --private tailscale --allow-tools read,search --detach
a2a-serve start claude --workspace ~/src/app --mount ~/docs:/docs:ro
```

**General flags**

| Flag | Default | Description |
|---|---|---|
| `--description string` |  | Agent description for its card |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--env NAME=value` |  | Extra variable for the harness: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME` |  | Name of an environment variable to pass to the harness, such as an API key (NAME); its value is read from your environment (or prompted) and stored in .a2a/state/secrets (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the harness, as shell words, such as "cloudcode acp"; replaces the harness's default command |
| `--image string` |  | Container image of the harness; only applies to --sandbox docker |
| `--sandbox string` |  | Where the harness runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--identity login=member\|owner` |  | Role of another user's network login: login=member\|owner, such as anna@github=owner (repeatable) |
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, lan, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), ngrok, none, or the URL of an Agent Mesh relay |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools kinds` |  | ACP tool kinds approved without asking: read, edit, delete, move, search, execute, think, fetch, switch_mode, other, or a glob (repeatable; commas also separate values) |
| `--deny-tools kinds` |  | ACP tool kinds denied without asking, like --allow-tools (repeatable; commas also separate values) |
| `--owner-chat channel[:who]` |  | Where an owner answers the approvals of tasks nobody watches: channel[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--dir string` |  | Agent directory (default: the current directory) |
| `--quiet` |  | Print less on the terminal: keep the harness image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

## a2a-serve start echo

Run a test server which replies with the message

```
a2a-serve start echo [flags]
```

**Examples**

```bash
a2a-serve start echo --port 9000
a2a send -a http://127.0.0.1:9000 hello
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertise-address string` |  | Agent endpoint to put in the agent card (default: the listen address) |
| `--card string` |  | Serve a custom agent card JSON file |
| `--card-compat` |  | Serve the agent card in a dual v0.3/v1.0 format |
| `--description string` |  | Agent description for the generated card |
| `--host string` | `127.0.0.1` | Bind address |
| `--name string` |  | Agent name for the generated card |
| `--port int` | `8080` | Listen port |
| `--protocol string` | `latest` | Protocol version: "latest" or "0.3" |
| `--quiet` |  | Print less on the terminal: don't log the traffic |
| `--transport string` | `rest` | Transport to serve: rest, jsonrpc or grpc |

## a2a-serve start exec

Run a test server, an agent driven by a script. For each message:

```
- the command runs with sh -c, with the message text on stdin, and
  A2A_TASK_ID and A2A_CONTEXT_ID in its environment, so it can keep
  state per task or conversation;
- its stdout is the reply artifact, sent when it exits, or streamed with
  --stream (line by line) or --chunk (split at a delimiter);
- a non-zero exit fails the task, with stderr as the message.
```

```
a2a-serve start exec <command> [flags]
```

**Examples**

```bash
a2a-serve start exec 'tr a-z A-Z'
a2a-serve start exec --stream 'for i in 1 2 3; do echo "step $i"; sleep 1; done'
a2a-serve start exec 'mkdir -p "/tmp/$A2A_CONTEXT_ID" && tee -a "/tmp/$A2A_CONTEXT_ID/log"'
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertise-address string` |  | Agent endpoint to put in the agent card (default: the listen address) |
| `--card string` |  | Serve a custom agent card JSON file |
| `--card-compat` |  | Serve the agent card in a dual v0.3/v1.0 format |
| `--chunk string` |  | Stream stdout in chunks split at this delimiter |
| `--description string` |  | Agent description for the generated card |
| `--host string` | `127.0.0.1` | Bind address |
| `--name string` |  | Agent name for the generated card |
| `--port int` | `8080` | Listen port |
| `--protocol string` | `latest` | Protocol version: "latest" or "0.3" |
| `--quiet` |  | Print less on the terminal: don't log the traffic |
| `--stream` |  | Stream stdout line by line |
| `--transport string` | `rest` | Transport to serve: rest, jsonrpc or grpc |

## a2a-serve start opencode

Start an opencode agent in this directory. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like `a2a-serve config set`.

```
a2a-serve start opencode [flags]
```

**Examples**

```bash
a2a-serve start opencode --name coder --secret OPENROUTER_API_KEY
a2a-serve start opencode --private tailscale --allow-tools read,search --detach
a2a-serve start opencode --workspace ~/src/app --mount ~/docs:/docs:ro
```

**General flags**

| Flag | Default | Description |
|---|---|---|
| `--description string` |  | Agent description for its card |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--env NAME=value` |  | Extra variable for the harness: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME` |  | Name of an environment variable to pass to the harness, such as an API key (NAME); its value is read from your environment (or prompted) and stored in .a2a/state/secrets (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the harness, as shell words, such as "cloudcode acp"; replaces the harness's default command |
| `--image string` |  | Container image of the harness; only applies to --sandbox docker |
| `--sandbox string` |  | Where the harness runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--identity login=member\|owner` |  | Role of another user's network login: login=member\|owner, such as anna@github=owner (repeatable) |
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, lan, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), ngrok, none, or the URL of an Agent Mesh relay |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools kinds` |  | ACP tool kinds approved without asking: read, edit, delete, move, search, execute, think, fetch, switch_mode, other, or a glob (repeatable; commas also separate values) |
| `--deny-tools kinds` |  | ACP tool kinds denied without asking, like --allow-tools (repeatable; commas also separate values) |
| `--owner-chat channel[:who]` |  | Where an owner answers the approvals of tasks nobody watches: channel[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--dir string` |  | Agent directory (default: the current directory) |
| `--quiet` |  | Print less on the terminal: keep the harness image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

## a2a-serve start proxy

Run a test server which forwards each request to the agent with the agent card URL &lt;agent-url&gt;, logging the traffic.

--auth takes a header value, which may end in a secret value: env:VAR reads the environment variable VAR, and - a prompt without echo or stdin.

```
a2a-serve start proxy <agent-url> [flags]
```

**Examples**

```bash
a2a-serve start proxy https://agent.example.com --auth "Bearer env:AGENT_TOKEN"
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertise-address string` |  | Agent endpoint to put in the agent card (default: the listen address) |
| `--auth string` |  | Shorthand for --svc-param Authorization=&lt;creds&gt; for the proxied agent; may end in env:VAR or - |
| `--card string` |  | Serve a custom agent card JSON file |
| `--card-compat` |  | Serve the agent card in a dual v0.3/v1.0 format |
| `--description string` |  | Agent description for the generated card |
| `--host string` | `127.0.0.1` | Bind address |
| `--insecure` |  | Reach the proxied agent over plaintext gRPC |
| `--name string` |  | Agent name for the generated card |
| `--port int` | `8080` | Listen port |
| `--protocol string` | `latest` | Protocol version: "latest" or "0.3" |
| `--quiet` |  | Print less on the terminal: don't log the traffic |
| `--svc-param key=value` |  | Service parameter key=value (repeatable) for the proxied agent |
| `--tenant string` |  | Tenant of the proxied agent |
| `--transport string` | `rest` | Transport to serve: rest, jsonrpc or grpc |
