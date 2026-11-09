# a2a-serve config

<!-- Generated from the command definitions; do not edit. -->

Show and change the settings in .a2a/config.yaml with the flags of
`a2a-serve start <agent>`. Every change is validated and printed as a diff. A
running agent applies the description, the access settings and the
approval settings at once; other settings wait for `a2a-serve restart`.

Without a subcommand, shows the settings.

```
a2a-serve config [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--as-command` |  | Print the a2a-serve commands that recreate the agent, with secrets as env:NAME |
| `--defaults` |  | Also show the settings left at their defaults, with the default values |

**Flags of every `a2a-serve config` command**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |

## a2a-serve config edit

Open a copy of .a2a/config.yaml in $VISUAL or $EDITOR (default vi). On save
it is validated and applied like `a2a-serve config set`; if it is invalid,
the file reopens with the errors on top, like crontab -e. A value typed into
a secret setting is stored in .a2a/state/secrets, and the file keeps a
secret:NAME reference. Messengers are changed with `a2a-serve messenger add`
and `a2a-serve messenger rm`.

```
a2a-serve config edit
```

## a2a-serve config set

Change settings with the flags of `a2a-serve start <agent>`, and the
operational settings only config set takes: limits, network-provider
options, backups and Google sign-in. Messengers are set with
`a2a-serve messenger add`. With --agent, config set also creates an
agent, to be started with `a2a-serve start`. Scalars are
overwritten. List entries are added, or replace the entry with the same key:
--secret by name, --env by NAME, --mount by destination, and the other
lists by value. Remove entries with `a2a-serve config unset`. A change to who
may call the agent, or to its networks, prints who may call afterwards.

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
| `--contact-refresh duration` |  | How often the server fetches the agent cards of contacts and peers again, a duration such as 30m or 1h (default 1h) |
| `--description string` |  | Agent description for its card |
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |
| `--task-ttl duration` |  | How long finished tasks are kept after their last update, a duration in hours or days such as 12h or 30d (default 30d); a chat conversation starts over when its last task is deleted |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-broad-mount path` |  | Let the Docker sandbox mount a path that is your home directory, / or a directory that holds your home directory; the agent can then read your credentials and the secrets of the agents in it (repeatable) |
| `--disable-mcp servers` |  | MCP servers of the project's .mcp.json the agent doesn't get (repeatable; commas also separate values) |
| `--env NAME=value` |  | Extra variable for the agent: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME[=value]` |  | Environment variable to pass to the agent, such as an API key: NAME[=value], stored in .a2a/state/secrets outside config.yaml; the agent can read it, unless --inject-header or --inject-model-key adds it; without a value, read from your environment or prompted (secret value: env:VAR, -, or the value) (repeatable) |
| `--workspace src[:dst][:ro]` |  | Workspace directory: src[:dst][:ro], such as ~/src/app (default: the directory holding .a2a) |

**Sandbox flags**

| Flag | Default | Description |
|---|---|---|
| `--command words` |  | Command line of the agent, as shell words, such as "cloudcode acp"; replaces its default command |
| `--cpus string` |  | Container CPU limit, such as 2 or 1.5 (default 2) |
| `--egress mode` |  | Where the Docker sandbox may connect, a mode: internet (the public internet and the --egress-allow hosts), allowlist (only the --egress-allow hosts, which must include the API of the model provider) or unrestricted (Docker's own network, the LAN and the tailnet included) (default internet) |
| `--egress-allow rule` |  | Host the Docker sandbox may also reach, private ones included: a rule such as nas.lan, \*.example.com, 192.168.1.20 or 192.168.1.0/24, with an optional :port (repeatable; commas also separate values) |
| `--image string` |  | Container image of the agent; only applies to --sandbox docker |
| `--inject-header host=Name: value` |  | Header the server adds to the Docker sandbox's HTTPS requests to a host, so that the agent doesn't see the secret in it: host=Name: value, with $NAME for a variable of your environment, stored in .a2a/state/secrets, such as 'api.github.com=Authorization: Bearer $GITHUB_TOKEN'; keyed by host (repeatable) |
| `--inject-model-key provider=NAME` |  | API key the server adds to the Docker sandbox's requests to a model provider; the agent gets a placeholder in the variable: provider=NAME, with provider one of anthropic, gemini, openai, openrouter and NAME a variable of your environment, stored in .a2a/state/secrets, such as openrouter=OPENROUTER_API_KEY; keyed by provider (repeatable) |
| `--memory string` |  | Container memory limit of at least 6m, such as 4g or 512m (default 4g) |
| `--pids int` |  | Container process limit (default 512) |
| `--sandbox string` |  | Where the agent runs: docker (default) or local |

**Network flags**

| Flag | Default | Description |
|---|---|---|
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay |
| `--relay-token string` |  | Join token of an Agent Mesh relay, to join without the sign-in link (secret value: env:VAR, -, or the value) |
| `--tailscale-authkey string` |  | Tailscale auth key, to join without the login link (secret value: env:VAR, -, or the value) |
| `--tailscale-control-url string` |  | Headscale URL, such as https://headscale.example.com, instead of Tailscale's control server |

**Access flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-email email` |  | People who may call the agent as members by a verified email, on any network: an email, or \*@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL (repeatable; commas also separate values) |
| `--google-client-id string` |  | Client ID of your own Google app for sign-in (optional) |
| `--google-client-secret string` |  | Client secret of your own Google app (secret value: env:VAR, -, or the value) |
| `--guest-secrets names` |  | Secrets the agent container of guests gets: names from --secret, all, none, or NAME=secret:OTHER to pass the secret OTHER as NAME, such as a guest key of its own; replaces the list (default none) (repeatable; commas also separate values) |
| `--guest-storage string` |  | What the agent container of guests, the callers nobody vouched for, gets of the workspace: shared, read-only, own (a private copy) or empty (default read-only) |
| `--private-allow-incoming string` |  | Who on the private network may call: everyone (or anyone) as members, or paired for only you and your pairings (default everyone) |
| `--private-allow-outgoing string` |  | Whom on the private network the agent may call: everyone, or contacts for only its contacts (default contacts) |
| `--public-allow-incoming string` |  | Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone as guests (everyone works too) (default paired) |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools entries` |  | Tool entries approved without asking: ACP tool kinds (read, edit, delete, move, search, execute, think, fetch, switch_mode, other), tool:&lt;server&gt;.&lt;tool&gt; for MCP tools, or command:&lt;command&gt; for shell commands; \* matches any text, and the most specific entry of --allow-tools and --deny-tools decides (repeatable; commas also separate values) |
| `--deny-tools entries` |  | Tool entries denied without asking, in the forms of --allow-tools (repeatable; commas also separate values) |
| `--owner-chat messenger[:who]` |  | Where an owner answers the approvals of tasks nobody watches: messenger[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dry-run` |  | Print the diff without saving |
| `--restart` |  | Restart the running agent if the change, or one saved before, needs it |

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
| `--as-command` |  | Print the a2a-serve commands that recreate the agent, with secrets as env:NAME |
| `--defaults` |  | Also show the settings left at their defaults, with the default values |

## a2a-serve config unset

Remove entries from a list setting, or the whole list without entries, or
reset a setting to its default. The setting is the flag name without --.
Entries are named by their key: a secret or env by name, a mount by its
destination, others by value. Unsetting a secret also deletes its value
from .a2a/state/secrets. Messengers are removed with `a2a-serve messenger rm`.

Settings: agent, allow-broad-mount, allow-email, allow-tools, backup, backup-interval, backup-key, backup-retention, card-skills, command, contact-refresh, cpus, deny-tools, description, disable-mcp, egress, egress-allow, env, google-client-id, google-client-secret, guest-secrets, guest-storage, image, inject-header, inject-model-key, memory, model, mount, name, owner-chat, pids, port, private, private-allow-incoming, private-allow-outgoing, public, public-allow-incoming, relay-token, sandbox, sandbox-rules, secret, tailscale-authkey, tailscale-control-url, task-ttl, workspace

```
a2a-serve config unset <setting> [<entry>...] [flags]
```

**Examples**

```bash
a2a-serve config unset secret OPENROUTER_API_KEY
a2a-serve config unset env DEBUG
a2a-serve config unset mount /docs
a2a-serve config unset allow-email '*@example.com'
a2a-serve config unset private --restart
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dry-run` |  | Print the diff without saving |
| `--restart` |  | Restart the running agent if the change, or one saved before, needs it |
