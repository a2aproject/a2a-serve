# a2a-serve pair

<!-- Generated from the command definitions; do not edit. -->

On the agent's side, "invite" creates a pairing and prints its one-time code;
"list", "get", "set" and "rm" manage the pairings. The caller redeems the code
with `a2a-serve pair join <agent-url> <code>`, or by sending "join &lt;code&gt;"
to the agent's bot.

On the caller's side, "join" saves the agent's token to
~/.config/a2a-serve/credentials.json, which the a2a CLI sends with requests to
that agent. Outside an agent directory, or with --joined, "list" shows the
agents you joined, and "get" and "rm" take an agent URL.

```
a2a-serve pair <command>
```

**Examples**

```bash
a2a-serve pair invite anna
a2a-serve pair join https://coder.tail1234.ts.net 3MZ8-LQ0D
a2a-serve pair list
a2a-serve pair set anna --role owner
a2a-serve pair rm anna
```

## a2a-serve pair get

Show a pairing: its role, tokens, linked accounts and pending codes, or an agent you joined

```
a2a-serve pair get <name|agent-url>
```

## a2a-serve pair invite

Create a pairing called &lt;name&gt; and print its code: 8 characters, valid once
for 15 minutes. The caller redeems it for a token with
`a2a-serve pair join <agent-url> <code>`, or links a messenger account by
sending "join &lt;code&gt;" to the agent's bot. Inviting an existing name issues
another code, for another device or account; its tokens keep working.
`a2a-serve pair set` changes a pairing, and rotates its credentials.

With --webhook, &lt;name&gt; is a webhook node: an agent which only runs when woken,
such as a Grok Bot routine. The agent hosts it at &lt;agent-url&gt;/n/&lt;name&gt;, calls
it as &lt;name&gt; with the a2a CLI, and wakes it with a signed POST to the
webhook. The node pairs with the code the same way, answers with
`a2a-serve mailbox reply`, and can call the agent with its token. A header
value may end in a secret value, env:VAR or -, as in
"Authorization: Bearer env:TOKEN".

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

--advertised tells every agent on the private network about the node: they
list it in `a2a-serve peers` as &lt;name&gt;@&lt;this agent&gt; and can add it with
`a2a-serve contact add <name>@<this agent>`, with no code. Their calls go
through this agent, which keeps the webhook's credentials, and it treats
them like any other caller from the private network.

```
a2a-serve pair invite <name> [flags]
```

**Examples**

```bash
a2a-serve pair invite grok
a2a-serve pair invite anna --role owner
a2a-serve pair invite grok --webhook https://example.com/hooks/123 \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
a2a-serve pair invite grok --webhook https://example.com/hooks/123 --advertised
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertised` |  | Advertise the webhook node to the agents of the private network |
| `--description string` |  | Description for the node's card |
| `--role authenticated\|owner` | `authenticated` | Role: authenticated\|owner; authenticated callers see and approve their own tasks, owners every task |
| `--webhook string` |  | Host a webhook node: the URL the agent POSTs wakes to |
| `--webhook-header Name: value` |  | Header sent with every wake: Name: value, such as "Authorization: Bearer env:TOKEN"; the value may end in env:VAR or - (repeatable) |

## a2a-serve pair join

Exchange a pairing code from an agent's owner (a2a-serve pair invite) for a token.
The token is saved to ~/.config/a2a-serve/credentials.json. The a2a CLI
sends it with later requests to the same agent, such as a2a send, through the
credential helper that a2a-serve doctor --fix configures.

A code for a webhook node (a2a-serve pair invite --webhook) returns a node token
instead, which the a2a-serve mailbox commands use.

```
a2a-serve pair join <agent-url> <code>
```

**Examples**

```bash
a2a-serve pair join https://assistant.tail1234.ts.net 3MZ8-LQ0D
```

## a2a-serve pair list

In an agent directory, list the callers and webhook nodes paired with the
agent. Outside one, or with --joined, list the agents you joined with
`a2a-serve pair join`.

```
a2a-serve pair list [flags]
```

Aliases: `ls`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--joined` |  | List the agents you joined, also in an agent directory |

## a2a-serve pair rm

Revoke a pairing, remove a webhook node, or forget an agent you joined

```
a2a-serve pair rm <name|agent-url>
```

Aliases: `delete`

## a2a-serve pair set

Change a pairing in place: the role of a caller, or the webhook, headers,
description and advertising of a webhook node. Its tokens keep working.

--rotate revokes every token of the pairing and its pending codes, and
prints a new code; for a webhook node, it also changes the secret its wakes
are signed with. Linked messenger accounts stay linked.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve pair set <name> [flags]
```

**Examples**

```bash
a2a-serve pair set anna --role owner
a2a-serve pair set anna --rotate
a2a-serve pair set grok --webhook https://example.com/hooks/456 --advertised
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertised` |  | Advertise the webhook node to the agents of the private network |
| `--description string` |  | Description for the node's card |
| `--role authenticated\|owner` |  | Role of a caller: authenticated\|owner; authenticated callers see and approve their own tasks, owners every task |
| `--rotate` |  | Revoke every token of the pairing and print a new code |
| `--webhook string` |  | The URL the agent POSTs wakes to, for a webhook node |
| `--webhook-header Name: value` |  | Header sent with every wake, replacing the current ones: Name: value; the value may end in env:VAR or - (repeatable) |
