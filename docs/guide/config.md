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
A running agent applies `--description`, `card.skills`, `--private-allow-incoming`, `--public-allow-incoming`, `--private-allow-outgoing`, `--allow-email`, `--guest-storage`, `--guest-secrets`, `policy.sandbox`, `--allow-tools`, `--deny-tools`, `--disable-mcp`, `--owner-chat`, `--egress-allow` and `--contact-refresh` at once, and messengers too. Other settings apply after `a2a-serve restart`, or pass `--restart` to `config set` or `config unset`.
<!-- /generated:live-settings -->

| Command | What it does |
| --- | --- |
| `a2a-serve config show` | Shows every setting that is set, and with `--defaults` the others too. `--as-command` prints the commands that recreate the agent, and `-o yaml` prints the settings as YAML. |
| `a2a-serve config set <flags>` | Changes settings. `--dry-run` only prints the diff. |
| `a2a-serve config unset <setting> [<entry>…]` | Removes entries from a list, or resets a setting to its default. |
| `a2a-serve config edit` | Opens `config.yaml` in your editor and validates it on save. |

## Which command to use

Use `a2a-serve config set` for the settings you name, and for secret values. It stores secret values in `.a2a/state/secrets`.

Use `a2a-serve config edit` for several changes at once, for comments, and to repair a `config.yaml` that doesn't load. It reopens the file with the errors on top until the file is valid. When you give up, it keeps your edit in `.a2a/state/config-draft.yaml`, and the next `a2a-serve config edit` opens it.

Both commands check the settings. A change that adds a problem is refused. A change to a config that already has problems is saved, and the output lists what is still to fix, with the command that fixes it:

```console
$ a2a-serve config set --secret API_A
  secret value API_A stored
✓ saved .a2a/config.yaml
! still to fix before the agent starts: config.yaml: secret API_B is not stored: run `a2a-serve config set --secret API_B` with the variable set
```

Change messengers with `a2a-serve messenger add` and `a2a-serve messenger rm`. List settings such as `--secret`, `--env` and `--mount` replace an existing entry with the same name. [Secrets](secrets.md) covers `--secret` and the flags that take tokens.

`config set --agent opencode` creates an agent without starting it. Use it when the first start needs settings that `start` doesn't take, such as `--tailscale-authkey`.

On an existing agent, `config set --agent claude` switches the agent and keeps its other settings. It warns when the previous command, model or image stays set. Keep overrides you still need, or remove each with `a2a-serve config unset command`, `a2a-serve config unset model` or `a2a-serve config unset image`.
