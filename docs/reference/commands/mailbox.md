# a2a-serve mailbox

<!-- Generated from the command definitions; do not edit. -->

Commands a webhook node runs: an agent which only runs when woken, such as a
Grok Bot routine, hosted by a server ("a2a-serve pair invite --webhook" on its side).
The server holds the node's tasks in a mailbox until the node wakes. The node
pairs once with "a2a-serve pair join &lt;agent-url&gt; &lt;code&gt;", then answers each wake
with reply.

The node token comes from --token, $A2A_NODE_TOKEN, or the token
"a2a-serve pair join" saved. It includes the agent's URL.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve mailbox <command>
```

**Flags of every `a2a-serve mailbox` command**

| Flag | Default | Description |
|---|---|---|
| `--timeout duration` | `30s` | Timeout of each request to the server, a duration such as 30s or 2m |

## a2a-serve mailbox get

Show a task addressed to the node, with history and artifacts.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve mailbox get <task-id> [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--token string` |  | Node token (default: $A2A_NODE_TOKEN or the token saved by a2a-serve pair join) (secret value: env:VAR, -, or the value) |

## a2a-serve mailbox list

Show the node, and wakes not delivered yet or tasks still waiting for a reply.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve mailbox list [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--all` |  | List the last 200 wakes |
| `--token string` |  | Node token (default: $A2A_NODE_TOKEN or the token saved by a2a-serve pair join) (secret value: env:VAR, -, or the value) |

## a2a-serve mailbox reply

Post progress, a question or the result of a task.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve mailbox reply <task-id> [flags]
```

**Examples**

```bash
a2a-serve mailbox reply <task> --working "on it"
a2a-serve mailbox reply <task> --input-required "Which branch?"
a2a-serve mailbox reply <task> --final "Done" --artifact report.pdf
a2a-serve mailbox reply <task> --fail "Could not reach the repo"
a2a-serve mailbox reply <task> --reject "Not something this node does"
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--artifact stringArray` |  | Attach a file as an artifact (repeatable) |
| `--fail string` |  | Fail the task with this reason |
| `--final string` |  | Complete the task with this result |
| `--input-required string` |  | Ask the caller this question |
| `--key string` |  | Idempotency key (default: derived from the task and the event content) |
| `--reject string` |  | Reject the task with this reason |
| `--status string` |  | Task state: working, input-required, completed, failed, rejected; each also has a flag of its own |
| `--text string` |  | Status message, or the result for completed (with --status) |
| `--token string` |  | Node token (default: $A2A_NODE_TOKEN or the token saved by a2a-serve pair join) (secret value: env:VAR, -, or the value) |
| `--working string` |  | Report progress with this message |

## a2a-serve mailbox verify

Check that the wake body on stdin was signed by the agent hosting the node,
within 5m0s of now. Pass the value of the X-A2A-Serve-Signature
header with --signature. The key comes from the node token, so the command
makes no request. It fails for a wake that anyone else sent; don't act on one.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve mailbox verify --signature <header> < wake.json [flags]
```

**Examples**

```bash
printf '%s' "$WAKE_BODY" | a2a-serve mailbox verify --signature "$WAKE_SIGNATURE"
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--signature string` |  | Value of the X-A2A-Serve-Signature header of the wake |
| `--token string` |  | Node token (default: $A2A_NODE_TOKEN or the token saved by a2a-serve pair join) (secret value: env:VAR, -, or the value) |
