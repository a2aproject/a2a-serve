# a2a-serve config

<!-- Generated from the command definitions; do not edit. -->

Show and change the settings in .a2a/config.yaml with the flags of
`a2a-serve start <agent>`. Every change is validated and printed as a diff. A
running agent applies the description and the approval settings at once;
other settings wait for `a2a-serve restart`.

Without a subcommand, shows the settings.

```
a2a-serve config [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--as-command` |  | Print the a2a-serve commands which recreate the agent, with secrets as env:NAME |
| `--defaults` |  | Also show the settings left at their defaults, with the default values |

## a2a-serve config edit

Open a copy of .a2a/config.yaml in $VISUAL or $EDITOR (default vi). On save
it is validated and applied like `a2a-serve config set`; if it is invalid,
the file reopens with the errors on top, like crontab -e. Secret values
aren't in config.yaml: store them with `a2a-serve config set --secret NAME`.

```
a2a-serve config edit
```

## a2a-serve config set

Change settings with the flags of `a2a-serve start <agent>`, and the
operational settings only config set takes: limits, network-provider
options, backups and Google sign-in. Messengers are set with
`a2a-serve channel add`. With --agent, config set also creates an
agent, to be started with `a2a-serve start`. Scalars are
overwritten. List entries are added, or replace the entry with the same key:
--secret by name, --env by NAME, --mount by destination, --identity by login,
and --allow-tools and --deny-tools by value. Remove entries with
`a2a-serve config unset`.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve config set <flags> [flags]
```

**Examples**

```bash
a2a-serve config set --sandbox local --allow-tools read,search --secret ANTHROPIC_API_KEY
a2a-serve config set --tailscale-authkey - --restart
a2a-serve config set --mount ~/docs:/docs:ro --dry-run
```

**General flags**

| Flag | Default | Description |
|---|---|---|
| `--agent string` |  | Agent: claude, opencode, or acp with --command |
| `--backup string` |  | Take backup snapshots to a directory, gs://bucket/prefix or s3://bucket/prefix |
| `--backup-interval duration` |  | Time between snapshots, a duration such as 30m or 1h (default 24h) |
| `--backup-key string` |  | Passphrase the snapshots are encrypted with; keep a copy away from the backups (secret value: env:VAR, -, or the value) |
| `--backup-retention duration` |  | How long snapshots are kept, a duration in hours or days such as 48h or 7d; the latest is always kept (default 7d) |
| `--description string` |  | Agent description for its card |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |
| `--task-ttl duration` |  | How long tasks are kept after their last update, a duration in hours or days such as 12h or 30d (default 30d) |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--env NAME=value` |  | Extra variable for the agent: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME` |  | Name of an environment variable to pass to the agent, such as an API key (NAME); its value is read from your environment (or prompted) and stored in .a2a/state/secrets (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the agent, as shell words, such as "cloudcode acp"; replaces its default command |
| `--cpus string` |  | Container CPU limit, such as 2 or 1.5 (default 2) |
| `--image string` |  | Container image of the agent; only applies to --sandbox docker |
| `--memory string` |  | Container memory limit, such as 4g or 512m (default 4g) |
| `--pids int` |  | Container process limit (default 512) |
| `--sandbox string` |  | Where the agent runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-email email` |  | People who may call the agent by a verified email, on any network: an email, or \*@domain for a Google Workspace domain or mail that passes DMARC; turns on Google sign-in (repeatable; commas also separate values) |
| `--google-client-id string` |  | Client ID of your own Google app for sign-in (optional) |
| `--google-client-secret string` |  | Client secret of your own Google app (secret value: env:VAR, -, or the value) |
| `--ngrok-authtoken string` |  | ngrok authtoken (secret value: env:VAR, -, or the value) |
| `--ngrok-url string` |  | Reserved ngrok domain, such as https://coder.ngrok.app |
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, lan, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), ngrok, none, or the URL of an Agent Mesh relay |
| `--relay-token string` |  | Join token of an Agent Mesh relay, to join without the sign-in link (secret value: env:VAR, -, or the value) |
| `--tailscale-authkey string` |  | Tailscale auth key, to join without the login link (secret value: env:VAR, -, or the value) |
| `--tailscale-control-url string` |  | Headscale URL, such as https://headscale.example.com, instead of Tailscale's control server |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools kinds` |  | ACP tool kinds approved without asking: read, edit, delete, move, search, execute, think, fetch, switch_mode, other, or a glob (repeatable; commas also separate values) |
| `--deny-tools kinds` |  | ACP tool kinds denied without asking, like --allow-tools (repeatable; commas also separate values) |
| `--owner-chat channel[:who]` |  | Where an owner answers the approvals of tasks nobody watches: channel[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dry-run` |  | Print the diff without saving |
| `--restart` |  | Restart the running agent if a change needs it |

## a2a-serve config show

Show the effective settings, and those waiting for a restart

```
a2a-serve config show [flags]
```

**Examples**

```bash
a2a-serve config show
a2a-serve config show --defaults -o yaml
a2a-serve config show --as-command
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--as-command` |  | Print the a2a-serve commands which recreate the agent, with secrets as env:NAME |
| `--defaults` |  | Also show the settings left at their defaults, with the default values |

## a2a-serve config unset

Remove entries from a list setting, or the whole list without entries, or
reset a setting to its default. The setting is the flag name without --.
Entries are named by their key: a secret or env by name, a mount by its
destination, an identity by login. Unsetting a secret also deletes its value
from .a2a/state/secrets. Messengers are removed with `a2a-serve channel rm`.

Settings: agent, allow-email, allow-tools, backup, backup-interval, backup-key, backup-retention, command, cpus, deny-tools, description, env, google-client-id, google-client-secret, image, memory, mount, name, ngrok-authtoken, ngrok-url, owner-chat, pids, port, private, public, relay-token, sandbox, secret, tailscale-authkey, tailscale-control-url, task-ttl, workspace

```
a2a-serve config unset <setting> [<entry>...] [flags]
```

**Examples**

```bash
a2a-serve config unset secret OPENROUTER_API_KEY
a2a-serve config unset env DEBUG
a2a-serve config unset mount /docs
a2a-serve config unset private
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dry-run` |  | Print the diff without saving |
