# a2a-serve contact

<!-- Generated from the command definitions; do not edit. -->

Manage the agents this agent may call

```
a2a-serve contact <command>
```

## a2a-serve contact add

Let the agent call another agent with the a2a CLI. Without --url the
agent is looked up by name on the private network, where it recognizes this
agent by its network identity. With --url, pair with a code from the other
agent's owner, or pass a token. A pairing another agent of the private
network advertises, such as grok@ben from `a2a-serve peers`, needs neither:
calls to it go through that agent.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve contact add <name> [flags]
```

**Examples**

```bash
a2a-serve contact add coder
a2a-serve contact add grok@ben
a2a-serve contact add grok --url https://grok.example.com --code K7Q2-9XF4
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--code string` |  | A pairing code from the other agent's owner |
| `--token string` |  | A token for the other agent (secret value: env:VAR, -, or the value) |
| `--url string` |  | The agent's URL |

## a2a-serve contact list

List contacts

```
a2a-serve contact list
```

Aliases: `ls`

## a2a-serve contact rm

Remove a contact

```
a2a-serve contact rm <name>
```

Aliases: `delete`
