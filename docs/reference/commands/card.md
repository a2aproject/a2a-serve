# a2a-serve card

<!-- Generated from the command definitions; do not edit. -->

Print the agent card as callers see it, in JSON. Each URL serves its own
card, which differs in the URL and in the credentials it asks for. Without
--url, the card of the URL that reaches furthest: public, then private, then
local. --url local also works while the agent is stopped, once its first
start picked its port.

The skills of the card come from card.skills in config.yaml; see the
configuration reference.

```
a2a-serve card [flags]
```

**Examples**

```bash
a2a-serve card
a2a-serve card --url=private
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |
| `--url scope` |  | Print the card of the agent's scope URL: local, private or public (default: the one that reaches furthest) |
