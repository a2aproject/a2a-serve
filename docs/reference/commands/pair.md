# a2a-serve pair

<!-- Generated from the command definitions; do not edit. -->

On the agent's side, "invite" creates a pairing and prints its one-time code;
"list", "get", "set" and "rm" manage the pairings. The caller redeems the code
with `a2a-serve pair join <agent-url> <code>`, or by sending "join &lt;code&gt;"
to the agent's bot.

On the caller's side, "join" saves the agent's token to
~/.config/a2a-serve/credentials.json, which the a2a CLI sends with requests to
that agent. "list" also shows the agents you joined, and "get" and "rm" take
an agent URL.

```
a2a-serve pair <command>
```

**Examples**

```bash
a2a-serve pair invite anna
a2a-serve pair join https://coder.tail1234.ts.net 3MZ8-LQ0D
a2a-serve pair list
a2a-serve pair set anna --owner
a2a-serve pair rm anna
```

**Flags of every `a2a-serve pair` command**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |

## a2a-serve pair get

Show a pairing: its kind, role, tokens, linked accounts and pending codes, or an agent you joined

```
a2a-serve pair get <name|agent-url>
```

## a2a-serve pair invite

Create a pairing called &lt;name&gt; and print its code: 8 characters, valid once
for 15 minutes. The caller redeems it for a token with
`a2a-serve pair join <agent-url> <code>`, or links a messenger account by
sending "join &lt;code&gt;" to the agent's bot. Another agent redeems it with
`a2a-serve contact add --code`. The first redemption sets the pairing's kind:
person, or agent for a contact. Inviting an existing name issues another
code, for another device or account; its tokens keep working. A new name has
up to 64 letters, digits, dots, hyphens or underscores, starts with a letter
or digit, and differs from the existing names by more than case.
`a2a-serve pair set` changes a pairing, and rotates its credentials. A paired
caller can approve commands in their own tasks, in an agent container of
their own, which gets the shared workspace and the agent's --secret values.
--storage and --secrets narrow what it gets, such as to a read-only
workspace or a key of its own.

With --webhook, &lt;name&gt; is a webhook node: an agent that only runs when woken,
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
a2a-serve pair invite anna --owner
a2a-serve pair invite ben --storage own --secrets ANTHROPIC_API_KEY
a2a-serve pair invite grok --webhook https://example.com/hooks/123 \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
a2a-serve pair invite grok --webhook https://example.com/hooks/123 --advertised
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertised` |  | Advertise the webhook node to the agents of the private network |
| `--description string` |  | Description for the node's card |
| `--owner` |  | Make the pairing an owner, who sees and approves every task; others see and approve their own |
| `--secrets names` |  | Narrow the secrets the pairing's agent container gets to names from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME (default all) |
| `--storage string` |  | Narrow what the pairing's agent container gets of the workspace: shared, read-only, own (a private copy) or empty (default shared) |
| `--webhook string` |  | Host a webhook node: the URL the agent POSTs wakes to |
| `--webhook-header Name: value` |  | Header sent with every wake: Name: value, such as "Authorization: Bearer env:TOKEN"; the value may end in env:VAR or - (repeatable) |

## a2a-serve pair join

Exchange a pairing code from an agent's owner (a2a-serve pair invite) for a token.
The token is saved to ~/.config/a2a-serve/credentials.json. The a2a CLI
sends it with later requests to the same agent, such as a2a send, through the
credential helper that a2a-serve doctor --fix configures.

Joining an agent you joined before replaces your saved token, so you lose
access to what you did with it. The command refuses that without --replace.
A code for a webhook node replaces nothing, so --replace is safe with it.

A code for a webhook node (a2a-serve pair invite --webhook) returns a node token
instead, which the a2a-serve mailbox commands use.

```
a2a-serve pair join <agent-url> <code> [flags]
```

**Examples**

```bash
a2a-serve pair join https://assistant.tail1234.ts.net 3MZ8-LQ0D
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--replace` |  | Join an agent you joined before, replacing the saved token |
| `--timeout duration` | `1m0s` | Timeout of the request to the other agent, a duration such as 30s or 2m |

## a2a-serve pair list

List the pairings of the agent with their kind and role: people, devices,
other agents and webhook nodes, and the built-in local pairing, which is you.
Then list the agents you joined with `a2a-serve pair join`. Run it in an agent
directory; --joined lists only the agents you joined, from any directory.

```
a2a-serve pair list [flags]
```

Aliases: `ls`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--invited` |  | List only the pairings of the agent |
| `--joined` |  | List only the agents you joined |

## a2a-serve pair rm

Delete a pairing with its tokens, linked messenger accounts, pending
codes, the schedules that run as it, and its tasks, approvals and
conversations, or remove a webhook node with its schedules. Its
conversations go with your messages in them. Its running tasks are
canceled, its agent process stops, and its agent home directory is
deleted. A new pairing with the same name starts with none of them. The
person may still call the agent through the private network or an
--allow-email rule; the command says so, and prints who may call
afterwards.

`a2a-serve pair rm local` revokes the token of this machine and your linked
chat accounts, and keeps the pairing; the next start issues a new token.

With an agent URL, forget the token you saved with `a2a-serve pair join`.

With --email, revoke the Google sign-ins of a person. They can sign in
again while an --allow-email rule matches their email; remove the rule with
`a2a-serve config unset allow-email` to stop that.

```
a2a-serve pair rm <name|agent-url> [flags]
```

Aliases: `delete`

**Examples**

```bash
a2a-serve pair rm anna
a2a-serve pair rm https://coder.tail1234.ts.net
a2a-serve pair rm --email anna@example.com
a2a-serve pair rm anna --dry-run
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dry-run` |  | Print what would be removed without removing it |
| `--email address` |  | Revoke the Google sign-ins of the email address |

## a2a-serve pair set

Change a pairing in place: whether it is an owner and its kind (person,
device or agent), what its agent container gets, or the webhook, headers,
description and advertising of a webhook node. Its tokens keep working, and
a change applies to their next request. A change of --storage or --secrets
applies to its agent container once its current turn ends.

--rotate revokes every token of the pairing and its pending codes, and
prints a new code. The caller joins again with the code and --replace, and
keeps its conversations and tasks. For a webhook node, --rotate also
changes the secret its wakes are signed with. Linked messenger accounts
stay linked.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve pair set <name> [flags]
```

**Examples**

```bash
a2a-serve pair set anna --owner
a2a-serve pair set anna --owner=false
a2a-serve pair set laptop --kind device
a2a-serve pair set ben --storage read-only --secrets none
a2a-serve pair set ben --secrets ANTHROPIC_API_KEY=secret:BEN_ANTHROPIC_KEY
a2a-serve pair set anna --rotate
a2a-serve pair set grok --webhook https://example.com/hooks/456 --advertised
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--advertised` |  | Advertise the webhook node to the agents of the private network |
| `--description string` |  | Description for the node's card |
| `--kind person\|device\|agent` |  | Kind of the pairing: person\|device\|agent |
| `--owner` |  | Make the pairing an owner, who sees and approves every task, or not with --owner=false |
| `--rotate` |  | Revoke every token of the pairing and print a new code |
| `--secrets names` |  | Narrow the secrets the pairing's agent container gets to names from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME (default all) |
| `--storage string` |  | Narrow what the pairing's agent container gets of the workspace: shared, read-only, own (a private copy) or empty (default shared) |
| `--webhook string` |  | The URL the agent POSTs wakes to, for a webhook node |
| `--webhook-header Name: value` |  | Header sent with every wake, replacing the current ones: Name: value; the value may end in env:VAR or - (repeatable) |
