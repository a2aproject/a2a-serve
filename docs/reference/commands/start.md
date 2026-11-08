# a2a-serve start

<!-- Generated from the command definitions; do not edit. -->

Without an argument, start the agent saved in the nearest .a2a directory
(or --dir). On a first run in a terminal, it asks a few questions instead.

With an agent, create it in this directory on the first run, or apply
the flags to the existing agent like `a2a-serve config set`, then start it.

echo, proxy and exec run a local test server instead, without an agent
directory.

```
a2a-serve start [<agent>|echo|proxy|exec] [flags]
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
| `--quiet` |  | Print less on the terminal: keep the agent image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |

## a2a-serve start acp

Start any ACP agent (--command) in this directory. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like `a2a-serve config set`. In a
subdirectory of an agent, pass --dir to create a separate agent while that
agent is stopped. The Docker sandbox refuses your home directory or / as the
workspace unless --allow-broad-mount names it.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

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
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-broad-mount path` |  | Let the Docker sandbox mount a path that is your home directory, / or a directory that holds your home directory; the agent can then read your credentials and the secrets of the agents in it (repeatable) |
| `--disable-mcp servers` |  | MCP servers of the project's .mcp.json the agent doesn't get (repeatable; commas also separate values) |
| `--env NAME=value` |  | Extra variable for the agent: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME[=value]` |  | Environment variable to pass to the agent, such as an API key: NAME[=value], stored in .a2a/state/secrets; without a value, read from your environment or prompted (secret value: env:VAR, -, or the value) (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the ACP agent, as shell words, such as "gemini --experimental-acp" |
| `--image string` |  | Container image of the agent; only applies to --sandbox docker |
| `--sandbox string` |  | Where the agent runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay |

**Access flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-email email` |  | People who may call the agent as members by a verified email, on any network: an email, or \*@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL (repeatable; commas also separate values) |
| `--guest-secrets names` |  | Secrets the agent container of guests gets: names from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME, such as a guest key of its own; replaces the list (default none) (repeatable; commas also separate values) |
| `--guest-storage string` |  | What the agent container of guests, the callers nobody vouched for, gets of the workspace: shared, read-only, own (a private copy) or empty (default read-only) |
| `--private-allow-incoming string` |  | Who on the private network may call: everyone (or anyone) as members, or paired for only you and your pairings (default everyone) |
| `--private-allow-outgoing string` |  | Whom on the private network the agent may call: everyone, or contacts for only its contacts (default contacts) |
| `--public-allow-incoming string` |  | Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone as guests (everyone works too) (default paired) |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools entries` |  | Tool entries approved without asking: ACP tool kinds (read, edit, delete, move, search, execute, think, fetch, switch_mode, other), tool:&lt;server&gt;.&lt;tool&gt; for MCP tools, or command:&lt;command&gt; for shell commands; \* matches any text, and the most specific entry of --allow-tools and --deny-tools decides (repeatable; commas also separate values) |
| `--deny-tools entries` |  | Tool entries denied without asking, in the forms of --allow-tools (repeatable; commas also separate values) |
| `--owner-chat messenger[:who]` |  | Where an owner answers the approvals of tasks nobody watches: messenger[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--dir string` |  | Agent directory (default: the current directory) |
| `--quiet` |  | Print less on the terminal: keep the agent image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

## a2a-serve start claude

Start a claude agent in this directory. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like `a2a-serve config set`. In a
subdirectory of an agent, pass --dir to create a separate agent while that
agent is stopped. The Docker sandbox refuses your home directory or / as the
workspace unless --allow-broad-mount names it.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

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
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-broad-mount path` |  | Let the Docker sandbox mount a path that is your home directory, / or a directory that holds your home directory; the agent can then read your credentials and the secrets of the agents in it (repeatable) |
| `--disable-mcp servers` |  | MCP servers of the project's .mcp.json the agent doesn't get (repeatable; commas also separate values) |
| `--env NAME=value` |  | Extra variable for the agent: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME[=value]` |  | Environment variable to pass to the agent, such as an API key: NAME[=value], stored in .a2a/state/secrets; without a value, read from your environment or prompted (secret value: env:VAR, -, or the value) (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the agent, as shell words, such as "cloudcode acp"; replaces its default command |
| `--image string` |  | Container image of the agent; only applies to --sandbox docker |
| `--sandbox string` |  | Where the agent runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay |

**Access flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-email email` |  | People who may call the agent as members by a verified email, on any network: an email, or \*@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL (repeatable; commas also separate values) |
| `--guest-secrets names` |  | Secrets the agent container of guests gets: names from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME, such as a guest key of its own; replaces the list (default none) (repeatable; commas also separate values) |
| `--guest-storage string` |  | What the agent container of guests, the callers nobody vouched for, gets of the workspace: shared, read-only, own (a private copy) or empty (default read-only) |
| `--private-allow-incoming string` |  | Who on the private network may call: everyone (or anyone) as members, or paired for only you and your pairings (default everyone) |
| `--private-allow-outgoing string` |  | Whom on the private network the agent may call: everyone, or contacts for only its contacts (default contacts) |
| `--public-allow-incoming string` |  | Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone as guests (everyone works too) (default paired) |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools entries` |  | Tool entries approved without asking: ACP tool kinds (read, edit, delete, move, search, execute, think, fetch, switch_mode, other), tool:&lt;server&gt;.&lt;tool&gt; for MCP tools, or command:&lt;command&gt; for shell commands; \* matches any text, and the most specific entry of --allow-tools and --deny-tools decides (repeatable; commas also separate values) |
| `--deny-tools entries` |  | Tool entries denied without asking, in the forms of --allow-tools (repeatable; commas also separate values) |
| `--owner-chat messenger[:who]` |  | Where an owner answers the approvals of tasks nobody watches: messenger[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--dir string` |  | Agent directory (default: the current directory) |
| `--quiet` |  | Print less on the terminal: keep the agent image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

## a2a-serve start echo

Run a test server that replies with the message

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
| `--no-traffic-log` |  | Don't log the requests and responses on the terminal |
| `--port int` | `8080` | Listen port |
| `--protocol string` | `latest` | Protocol version: "latest" or "0.3" |
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
| `--no-traffic-log` |  | Don't log the requests and responses on the terminal |
| `--port int` | `8080` | Listen port |
| `--protocol string` | `latest` | Protocol version: "latest" or "0.3" |
| `--stream` |  | Stream stdout line by line |
| `--transport string` | `rest` | Transport to serve: rest, jsonrpc or grpc |

## a2a-serve start opencode

Start an opencode agent in this directory. The first run saves the settings to .a2a/config.yaml; in an
existing agent, the flags change them like `a2a-serve config set`. In a
subdirectory of an agent, pass --dir to create a separate agent while that
agent is stopped. The Docker sandbox refuses your home directory or / as the
workspace unless --allow-broad-mount names it.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

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
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-broad-mount path` |  | Let the Docker sandbox mount a path that is your home directory, / or a directory that holds your home directory; the agent can then read your credentials and the secrets of the agents in it (repeatable) |
| `--disable-mcp servers` |  | MCP servers of the project's .mcp.json the agent doesn't get (repeatable; commas also separate values) |
| `--env NAME=value` |  | Extra variable for the agent: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME[=value]` |  | Environment variable to pass to the agent, such as an API key: NAME[=value], stored in .a2a/state/secrets; without a value, read from your environment or prompted (secret value: env:VAR, -, or the value) (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the agent, as shell words, such as "cloudcode acp"; replaces its default command |
| `--image string` |  | Container image of the agent; only applies to --sandbox docker |
| `--sandbox string` |  | Where the agent runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay |

**Access flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-email email` |  | People who may call the agent as members by a verified email, on any network: an email, or \*@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL (repeatable; commas also separate values) |
| `--guest-secrets names` |  | Secrets the agent container of guests gets: names from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME, such as a guest key of its own; replaces the list (default none) (repeatable; commas also separate values) |
| `--guest-storage string` |  | What the agent container of guests, the callers nobody vouched for, gets of the workspace: shared, read-only, own (a private copy) or empty (default read-only) |
| `--private-allow-incoming string` |  | Who on the private network may call: everyone (or anyone) as members, or paired for only you and your pairings (default everyone) |
| `--private-allow-outgoing string` |  | Whom on the private network the agent may call: everyone, or contacts for only its contacts (default contacts) |
| `--public-allow-incoming string` |  | Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone as guests (everyone works too) (default paired) |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools entries` |  | Tool entries approved without asking: ACP tool kinds (read, edit, delete, move, search, execute, think, fetch, switch_mode, other), tool:&lt;server&gt;.&lt;tool&gt; for MCP tools, or command:&lt;command&gt; for shell commands; \* matches any text, and the most specific entry of --allow-tools and --deny-tools decides (repeatable; commas also separate values) |
| `--deny-tools entries` |  | Tool entries denied without asking, in the forms of --allow-tools (repeatable; commas also separate values) |
| `--owner-chat messenger[:who]` |  | Where an owner answers the approvals of tasks nobody watches: messenger[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Run flags (not saved)**

| Flag | Default | Description |
|---|---|---|
| `--detach` |  | Run the agent in the background |
| `--dir string` |  | Agent directory (default: the current directory) |
| `--quiet` |  | Print less on the terminal: keep the agent image pull and build output in the log |
| `--service` |  | Install and start the agent as a launchd or systemd user service |

## a2a-serve start proxy

Run a test server that forwards each request to the agent with the agent card URL &lt;agent-url&gt;, logging the traffic.

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
| `--no-traffic-log` |  | Don't log the requests and responses on the terminal |
| `--port int` | `8080` | Listen port |
| `--protocol string` | `latest` | Protocol version: "latest" or "0.3" |
| `--svc-param key=value` |  | Service parameter key=value (repeatable) for the proxied agent |
| `--tenant string` |  | Tenant of the proxied agent |
| `--transport string` | `rest` | Transport to serve: rest, jsonrpc or grpc |
