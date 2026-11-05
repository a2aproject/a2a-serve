# a2a-serve recipe

<!-- Generated from the command definitions; do not edit. -->

Create an agent from a recipe: a directory with a committed
.a2a/config.yaml, and optionally AGENTS.md, skills, .mcp.json and
.a2a/recipe.yaml, which describes its MCP servers. The source is a tree URL
such as https://github.com/&lt;org&gt;/&lt;repo&gt;/tree/&lt;ref&gt;/&lt;path&gt;, a git URL with
an optional #&lt;ref&gt;:&lt;path&gt;, or a local directory. Git sources are fetched
with your git, so your SSH keys and credential helpers apply.

recipe shows what the agent will run and asks before it creates the agent
in ./&lt;name&gt;, or in --dir. It asks for the secrets the agent lacks and
whether to use each optional MCP server. The recipe sets what the agent
is; where it is reachable and who may call it stay yours. The flags of
`a2a-serve config set` apply on top of the recipe. Start the agent with
`a2a-serve start`.

Run recipe again in the agent's directory, without a source, to update the
agent to a newer version of its recipe: a branch moves to its newest
commit, a tag or a commit stays. The update keeps your changes. A file
changed both here and in the recipe stays as it is, and the recipe's
version is saved next to it as &lt;file&gt;.recipe.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve recipe [<source>] [flags]
```

**Examples**

```bash
a2a-serve recipe https://github.com/acme/agents/tree/v1.2/experts/elk --name elk
a2a-serve recipe git@github.com:acme/agents.git#v1.2:experts/elk --dry-run
a2a-serve recipe ./experts/elk --yes --disable-mcp kubernetes --secret GRAFANA_TOKEN
a2a-serve recipe --dry-run
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
| `--dir string` |  | Agent directory: a new agent's, or the agent to update (default: the agent here, or ./&lt;name&gt; for a new one) |
| `--dry-run` |  | Fetch the recipe and print the review without saving |
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |
| `--task-ttl duration` |  | How long tasks are kept after their last update, a duration in hours or days such as 12h or 30d (default 30d); a chat conversation starts over when its last task is deleted |
| `-y, --yes` |  | Create or update the agent without asking; the review still prints |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--disable-mcp servers` |  | MCP servers of the project's .mcp.json the agent doesn't get (repeatable; commas also separate values) |
| `--env NAME=value` |  | Extra variable for the agent: NAME=value, such as DEBUG=1; keyed by NAME (repeatable) |
| `--mount src[:dst][:ro]` |  | Extra directory to mount: src[:dst][:ro], such as ~/docs:/docs:ro; keyed by dst (repeatable) |
| `--secret NAME[=value]` |  | Environment variable to pass to the agent, such as an API key: NAME[=value], stored in .a2a/state/secrets; without a value, read from your environment or prompted (secret value: env:VAR, -, or the value) (repeatable) |
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
| `--port int` |  | Localhost port, 1-65535 (default: a free port, kept across runs) |
| `--private string` |  | Private network: tailscale, none, or the URL of an Agent Mesh relay such as https://relay.example.com |
| `--public string` |  | Public URL: tailscale (Funnel), none, or the URL of an Agent Mesh relay |
| `--relay-token string` |  | Join token of an Agent Mesh relay, to join without the sign-in link (secret value: env:VAR, -, or the value) |
| `--tailscale-authkey string` |  | Tailscale auth key, to join without the login link (secret value: env:VAR, -, or the value) |
| `--tailscale-control-url string` |  | Headscale URL, such as https://headscale.example.com, instead of Tailscale's control server |

**Access flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-email email` |  | People who may call the agent by a verified email, on any network: an email, or \*@domain for Google Workspace sign-in or mail that passes DMARC; turns on Google sign-in at the public URL (repeatable; commas also separate values) |
| `--google-client-id string` |  | Client ID of your own Google app for sign-in (optional) |
| `--google-client-secret string` |  | Client secret of your own Google app (secret value: env:VAR, -, or the value) |
| `--private-access string` |  | Who on the private network may call: everyone (or anyone), or paired for only you and your pairings (default everyone) |
| `--public-access string` |  | Who on the public URL and in direct chats may call: paired for only you and your pairings, or anyone (everyone works too) (default paired) |

**Approvals flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-tools kinds` |  | ACP tool kinds approved without asking: read, edit, delete, move, search, execute, think, fetch, switch_mode, other, or a glob (repeatable; commas also separate values) |
| `--deny-tools kinds` |  | ACP tool kinds denied without asking, like --allow-tools (repeatable; commas also separate values) |
| `--owner-chat channel[:who]` |  | Where an owner answers the approvals of tasks nobody watches: channel[:who], with who an owner pairing, such as telegram or telegram:anna (default: the first of telegram, discord, email with a linked owner) |
