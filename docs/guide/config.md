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

<!-- generated:live-settings -->
A running agent applies `--description`, `--private-access`, `--public-access`, `--allow-email`, `--allow-tools`, `--deny-tools`, `--disable-mcp` and `--owner-chat` at once, and messengers too. Other settings apply after `a2a-serve restart`, or pass `--restart` to `config set`.
<!-- /generated:live-settings -->

| Command | What it does |
| --- | --- |
| `a2a-serve config show` | Shows the settings. `--as-command` prints the commands that recreate the agent, and `-o yaml` prints the settings as YAML. |
| `a2a-serve config set <flags>` | Changes settings. `--dry-run` only prints the diff. |
| `a2a-serve config unset <setting> [<entry>…]` | Removes entries from a list, or resets a setting to its default. |
| `a2a-serve config edit` | Opens `config.yaml` in your editor and validates it on save. |

List settings are keyed: `--secret` and `--env` by name, `--mount` by destination, and other lists by value. Setting an existing entry replaces it. [Secrets](secrets.md) covers `--secret` and the flags that take tokens.

`config set --agent opencode` creates an agent without starting it. Use it when the first start needs settings that `start` doesn't take, such as `--tailscale-authkey`.
