# a2a-serve status

<!-- Generated from the command definitions; do not edit. -->

Show whether the agent runs, its URLs, channels and who may call it.

--url prints only the agent's URL which reaches furthest: public, then
private, then local. --url local, --url private and --url public pick one;
--url=local does the same. --url local also works while the agent is stopped, once its first start
picked its port.

```
a2a-serve status [flags]
```

**Examples**

```bash
a2a-serve status
a2a send -a "$(a2a-serve status --url)" "hello"
a2a-serve status --url=private
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |
| `--url scope` |  | Print only the agent's scope URL: local, private or public; without a value, the one which reaches furthest |
