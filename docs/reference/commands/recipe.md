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
| `--contact-refresh duration` |  | How often the server fetches the agent cards of contacts and peers again, a duration such as 30m or 1h (default 1h) |
| `--description string` |  | Agent description for its card |
| `--dir string` |  | Agent directory: a new agent's, or the agent to update (default: the agent here, or ./&lt;name&gt; for a new one) |
| `--dry-run` |  | Fetch the recipe and print the review without saving |
| `--model string` |  | Model of the agent, in its own format: provider/model for opencode, such as openrouter/~google/gemini-flash-latest, or a Claude model for claude, such as sonnet (default: the agent's) |
| `--name string` |  | Agent name, also its tailnet machine name (default: the directory name) |
| `--task-ttl duration` |  | How long finished tasks are kept after their last update, a duration in hours or days such as 12h or 30d (default 30d); a chat conversation starts over when its last task is deleted |
| `-y, --yes` |  | Create or update the agent without asking; the review still prints |

**Workspace flags**

| Flag | Default | Description |
|---|---|---|
| `--allow-broad-mount path` |  | Let the Docker sandbox mount a path that is your home directory, / or a directory that holds your home directory; the agent can then read your credentials and the secrets of the agents in it (repeatable) |
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
| `--egress mode` |  | Where the Docker sandbox may connect, a mode: internet (the public internet and the --egress-allow hosts), allowlist (only the --egress-allow hosts, which must include the API of the model provider) or unrestricted (Docker's own network, the LAN and the tailnet included) (default internet) |
| `--egress-allow rule` |  | Host the Docker sandbox may also reach, private ones included: a rule such as nas.lan, \*.example.com, 192.168.1.20 or 192.168.1.0/24, with an optional :port (repeatable; commas also separate values) |
| `--image string` |  | Container image of the agent; only applies to --sandbox docker |
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
