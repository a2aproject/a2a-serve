# Config

`a2a-serve config set` takes the same flags as `a2a-serve start opencode`, plus settings that only `config set` takes: container limits, network keys, backups, task retention and Google sign-in. The [config reference](../reference/commands/config.md) lists them all.

Each change is validated, saved and printed as a diff:

```console
$ a2a-serve config set --sandbox local --allow-tools read,search --secret ANTHROPIC_API_KEY
  sandbox      (default) → local
  secret       + ANTHROPIC_API_KEY (stored)
  allow-tools  + read, + search
✓ saved .a2a/config.yaml
! coder is running; restart to apply sandbox, secret: a2a-serve restart
```

The description, the owner chat, the allowed and denied tools, and messengers apply at once. Other settings apply after `a2a-serve restart`, or pass `--restart` to `config set`.

| Command | What it does |
| --- | --- |
| `a2a-serve config show` | Shows the settings. `--as-command` prints the commands that recreate the agent. |
| `a2a-serve config set <flags>` | Changes settings. `--dry-run` only prints the diff. |
| `a2a-serve config unset <setting> [<entry>…]` | Removes entries from a list, or resets a setting to its default. |
| `a2a-serve config edit` | Opens `config.yaml` in your editor and validates it on save. |

List settings are keyed: `--secret` and `--env` by name, `--mount` by destination, `--identity` by login. Setting an existing entry replaces it. `config unset secret OPENROUTER_API_KEY` removes the entry and its stored value.

`config set --agent opencode` creates an agent without starting it. Use it when the first start needs settings that `start` doesn't take, such as `--tailscale-authkey`.

## Secret values

Flags that take a secret read the value when you run the command and store it in `state/secrets`, so an agent running in the background doesn't need your shell's environment.

Every flag that takes a secret accepts these forms:

| Form | Meaning |
| --- | --- |
| `env:VAR` | The value of the environment variable `VAR`. |
| `-` | A hidden prompt on a terminal, or else all of stdin. |
| `secret:NAME` | A value already stored in `state/secrets`. |
| anything else | The value itself. |

`-` keeps tokens out of your shell history:

```sh
a2a-serve channel add telegram --token -
pass show coder/bot | a2a-serve channel add discord --token -
```

`pair invite --webhook-header` takes a whole header, which may end in `env:VAR` or `-`: `"Authorization: Bearer env:TOKEN"`.
