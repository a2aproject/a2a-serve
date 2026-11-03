# Agent server: `a2a serve run <harness>`

> [!WARNING]
> Pre-1.0. Commands, flags and `config.yaml` keys may change; stored state
> always migrates forward ([upgrades](upgrades.md)). See the
> [security page](security.md) for what the sandbox protects.

`a2a serve` runs one coding agent per process, started from its project
directory. The agent runs in Docker, is reachable over A2A, from Telegram,
Discord and email, and joins a private network with your other agents. Each
agent can also have a public URL.

How it works, with diagrams, is in the [architecture](agent-server-architecture.md).
Step-by-step walkthroughs are [lesson 1](../examples/01-agent-server/) (local
agent, pairing, approvals) and [lesson 2](../examples/02-tailscale-telegram-grok/)
(Tailscale, Telegram, a Grok webhook node).

Everything belongs to that agent: its workspace, channels, network identity and
pairings. Several agents on one machine are several directories and processes.
They find each other over the private network the same way agents on different
machines do.

## Installation

Install `a2a-serve` and the `a2a` CLI: callers use `a2a` to send tasks, and
harnesses use it to call other agents. Until an `a2a` release has
`--unix-socket` and credential helpers, install this version:

```sh
go install github.com/a2aproject/a2a-serve@latest
go install github.com/a2aproject/a2a-cli@v0.3.2-0.20261030153310-f90e4eb628cd
# go install names the binary `a2a-cli`; rename it to `a2a`
mv "$(command -v a2a-cli)" "$(dirname "$(command -v a2a-cli)")/a2a"
a2a-serve doctor --fix   # enable the plugin and the credential helper, check Docker
```

Without `doctor --fix`:

```sh
a2a plugin set-enabled true   # let `a2a` run a2a-serve as `a2a serve`
mkdir -p ~/.config/a2a-cli
grep -q '^credential-helper:' ~/.config/a2a-cli/config.yaml 2>/dev/null ||
  printf 'credential-helper:\n  - command: [a2a-serve, credential]\n' >> ~/.config/a2a-cli/config.yaml
```

Harnesses in Docker call other agents with the `a2a` of their image, which
carries the version this `a2a-serve` needs; only `--sandbox local` uses the
`a2a` on your `PATH`.

Run `a2a serve doctor` whenever something looks wrong: it checks the `a2a`
CLI and its config, Docker, and the agent of the current directory, and prints
the command which fixes each failure. If `a2a serve` fails with
`unknown command "serve" for "a2a"`, command plugins are disabled or
`a2a-serve` is not on `PATH`; run `a2a-serve doctor --fix`.

## Quick start

```sh
cd ~/src/app
a2a serve run opencode --name coder --secret OPENROUTER_API_KEY
```

The first run saves the flags to `.a2a/config.yaml` and starts the agent in
the foreground. Later runs, from the project or a subdirectory, are just
`a2a serve run`. Run `a2a serve run` without arguments in a new directory and
it asks a few questions instead. Each run prints which agent directory it uses,
and says when it creates one. Change settings later with the same flags, through
`a2a serve config set` (see [Changing settings](#changing-settings)).

On start, the server saves an owner token for its local URL in your
`~/.config/a2a-serve/credentials.json` and prints how to call it. The `a2a`
CLI attaches it through a2a-serve's credential helper, which
`a2a-serve doctor --fix` enables once in `~/.config/a2a-cli/config.yaml`:

```yaml
credential-helper:
  - command: [a2a-serve, credential]
```

Then, from another terminal on the same machine:

```sh
a2a send -a "$(a2a serve url --local)" "What does this project do?"
```

Anyone else needs an invitation:

```sh
a2a serve pair invite anna                    # in ~/src/app: prints a one-time code
a2a serve pair join <agent-url> <code>       # on anna's side: saves a token for that URL
```

`a2a send`, `a2a task …` and `a2a card get` attach the saved token
automatically.

Harnesses: `opencode` (`opencode acp`) and `claude` (Zed's
`claude-agent-acp`). `a2a serve run acp --command "<cmd>"` runs any agent that
speaks the [Agent Client Protocol](https://agentclientprotocol.com), and
`--command` also replaces a profile's command, such as
`a2a serve run opencode --command "cloudcode acp"`.

## The agent directory

```
~/src/app/
├── …project files…        # the workspace
└── .a2a/
    ├── config.yaml        # agent definition, no secrets; can be committed
    ├── .gitignore         # "state/"
    └── state/             # never committed
        ├── server.db      # SQLite: tasks, sessions, links, tokens, contacts, nodes
        ├── secrets        # tokens and API keys, mode 0600
        ├── tsnet/         # Tailscale identity: the same URL after restarts
        ├── home/          # harness home, mounted at /home/agent
        ├── server.log
        └── server.sock    # control socket for management commands
```

Management commands find the agent the way git finds a repository, or take
`--dir <path>` (`~/` is expanded). They talk to the running process over
`server.sock`, so changes apply without a restart; `--timeout` bounds each
call (default 60s).

`--secret VAR`, `--telegram env:VAR` and similar flags read values from your
shell when you run the command and store them in `state/secrets`. A detached
or service process doesn't depend on your shell's environment.

> [!WARNING]
> `config.yaml` decides what runs on your machine: `sandbox: local`,
> `command`, `image`, `mounts`, `env` and `secrets`. Running `a2a serve run` in a
> cloned repository runs whatever its `config.yaml` says, with access to the
> mounts and secrets it names. Read a `config.yaml` you didn't write before you
> start an agent from it, as you would a Makefile or a CI config.

## Running it

| Command | What it does |
| --- | --- |
| `a2a serve run` | Starts the agent in the foreground. `--quiet` keeps the harness image output in the log. |
| `a2a serve run --detach` | Starts it in the background. |
| `a2a serve run --service` | Installs and starts a systemd (Linux) or launchd (macOS) user service. |
| `a2a serve status` | URLs, channels, owner links, task counts. |
| `a2a serve url [--local\|--private\|--public]` | The agent's URL which reaches furthest: public, then private, then local. |
| `a2a serve logs [-f]` | The server and harness log. |
| `a2a serve stop [--uninstall]` | Stops it, detached or as a service; `--uninstall` also removes the service. |
| `a2a serve restart` | Stops it and starts it again in the same mode, detached or as a service. |

`restart` checks the settings first, so an agent which couldn't start again
isn't stopped. An agent running in the foreground of another terminal can't
be restarted from here: press Ctrl-C there and run `a2a serve run` again.

## Changing settings

`a2a serve config set` takes the flags of `a2a serve run <harness>`, so the first
run teaches the vocabulary for later changes. It also takes the operational
settings, which `run` doesn't: `--harness`, the container limits (`--memory`,
`--cpus`, `--pids`), the network-provider options (`--tailscale-control-url`,
`--tailscale-authkey`, `--ngrok-url`, `--ngrok-authtoken`, `--relay-token`),
backups (`--backup`, `--backup-key`, `--backup-interval`,
`--backup-retention`) and Google sign-in (`--allow-email`,
`--google-client-id`, `--google-client-secret`). Messengers are set with
`a2a serve channel add` (see [Messengers](#messengers)). With `--harness`,
`config set` also creates an agent without starting it, for the settings a
headless first start needs, such as `--tailscale-authkey`; `a2a serve run`
starts it. The help of `run <harness>` and `config set` groups the flags in
sections: General, Workspace, Sandbox, Network, Approvals, and Run (not
saved). Every change is validated
before it is saved, printed as a diff, and says whether it applies now or
after a restart:

```console
$ a2a serve config set --sandbox local --allow-tools read,search --secret ANTHROPIC_API_KEY
  sandbox      (default) → local
  secret       + ANTHROPIC_API_KEY (stored)
  allow-tools  + read, + search
✓ saved .a2a/config.yaml
! coder is running; restart to apply sandbox, secret: a2a serve restart
```

| Command | What it does |
| --- | --- |
| `a2a serve config` | The same as `config show`. |
| `a2a serve config show [--defaults] [--as-command]` | The effective settings, with secrets as names and their state (`stored`, `missing`), and the settings changed since the running server started. `--defaults` also lists unset settings; `--as-command` prints the commands which recreate the agent (`run <harness>`, then `config set` and `channel add` for the settings only they take), with secrets as `env:NAME`. `-o json` and `-o yaml` for scripts. |
| `a2a serve config set <flags> [--dry-run] [--restart]` | Validates, saves and prints the diff. `--dry-run` prints the diff only; `--restart` restarts a running agent when a change needs it. |
| `a2a serve config unset <setting> [<entry>…]` | Removes entries from a list setting, the whole list without entries, or resets a setting to its default. `<setting>` is the flag name without `--`. Messengers are removed with `channel rm`. |
| `a2a serve config edit` | Opens a copy of `config.yaml` in `$VISUAL` or `$EDITOR`. On save it is validated and applied; if it's invalid, it reopens with the errors on top, like `crontab -e`. |

Scalars such as `--name`, `--sandbox`, `--private` or `--port` are
overwritten by `set` and reset by `unset`; `--private none` is the same as
`unset private`. Lists are keyed, so setting an existing entry replaces it:

| List | Key | Example |
| --- | --- | --- |
| `--secret NAME` | name; setting it again re-reads the value | `config unset secret OPENROUTER_API_KEY` |
| `--env NAME=value` | `NAME` | `config unset env DEBUG` |
| `--mount src[:dst][:ro]` | destination | `config unset mount /docs` |
| `--identity login=role` | login | `config unset identity anna@example.com` |
| `--allow-tools`, `--deny-tools`, `--approver` | the value | `config unset allow-tools search` |

Every list flag can be repeated. `--allow-tools`, `--deny-tools`,
`--approver` and `--allow-email` also split values at commas; `--secret`,
`--env`, `--mount` and `--identity` don't, since their values may contain
commas. `--allow-tools` and `--deny-tools` take the ACP tool kinds `read`,
`edit`, `delete`, `move`, `search`, `execute`, `think`, `fetch`,
`switch_mode` and `other`, or a glob such as `*`.

`unset secret NAME`, and unsetting a channel, also delete the value from
`state/secrets`. `--secret NAME` is the name of an environment variable: it
reads `$NAME`, or prompts for it on a terminal when it isn't set, and stores
the value in `state/secrets`.

### Values

Every flag which takes a secret value accepts the same forms:

| Form | Meaning |
| --- | --- |
| `env:VAR` | The value of the environment variable `VAR`; an empty variable is an error. In agent settings, the value is stored under the name `VAR`. |
| `-` | On a terminal, a prompt without echo; otherwise all of stdin. One flag per command can read stdin. |
| `secret:NAME` | A value already stored in `.a2a/state/secrets`, in agent settings only. |
| anything else | The value itself. |

The flags are `--telegram`, `--discord`, `--email-password`,
`--tailscale-authkey`, `--ngrok-authtoken`, `--google-client-secret`,
`--relay-token` and `--backup-key` of the agent settings, `restore
--backup-key`, `channel add --token`, `contact add --token` and `mailbox
--token`. `-` keeps tokens out of the shell history:

```sh
a2a serve channel add telegram --token -
pass show coder/bot | a2a serve channel add discord --token -
```

Header values are the one exception: `pair invite --webhook-header` and
`proxy --auth` take a whole header value, which may end in `env:VAR` or `-`,
as in `"Authorization: Bearer env:TOKEN"`.

A running agent applies channels, the description, approvers and the
allowed and denied tools at once, through its control socket; other settings are saved and wait for
`a2a serve restart`, and `config show` and `status` list them.

`a2a serve run <harness>` in an existing agent goes through the same path, then
starts the agent unless it already runs. It refuses another harness:
`this agent runs opencode; switch with a2a serve config set --harness claude`.

An invalid `config.yaml` written by hand stops `a2a serve run` with an error
naming the setting; fix it with `config set`, `config unset` or `config edit`.

## Sandbox

By default the harness runs in Docker:

- the workspace, by default the directory holding `.a2a`, is mounted at its
  own path; `--workspace src[:dst][:ro]` picks another one, and
  `--mount src[:dst][:ro]` (repeatable) adds more directories;
- `.a2a` shows up as an empty read-only directory, so the harness can't read
  the secrets, tokens or config of its own server;
- `state/home` is mounted as `/home/agent`;
- the container runs as your user (as container root under rootless Docker,
  which maps to you), with all capabilities dropped, `no-new-privileges`,
  4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`).

The image is `ghcr.io/a2aproject/harness-<harness>:latest`. If it can't be
pulled, the server builds it from the Dockerfile bundled in the binary
(`internal/harness/images`). `--image` uses another image.

`--sandbox local` runs the harness on the host, for trusted setups or when
Docker isn't available. It runs as you, so it can read your
`~/.config/a2a-serve/credentials.json`, including the owner token of this
machine.

The harness calls other agents with the `a2a` CLI, through the server (see
[Agent to agent](#agent-to-agent)). How it reaches the server depends on the
sandbox:

| Sandbox | Connection to the server | `a2a` binary |
| --- | --- | --- |
| Docker on Linux | A Unix socket only the harness reaches, mounted at `/run/a2a/a2a.sock`. `A2ACLI_UNIX_SOCKET=<socket>` sends every request of the harness's `a2a` through it. | The image's `a2a`, at the version this `a2a-serve` needs, whatever `a2a` is on the host. The image tag names that version (`:a2a-<version>`); a custom `--image` needs a compatible `a2a` too. |
| Docker Desktop (macOS) | TCP with a key, since the socket can't be mounted. A credential helper in the harness home returns the key, and `A2ACLI_CREDENTIAL_HELPER=host.docker.internal=<helper>` sends it to that host only. | The image's `a2a`, as on Linux. |
| `--sandbox local` | The Unix socket, on the host. | The server's `a2a`: its directory is put first on the harness's `PATH`. |

The harness's `a2a` waits up to 5 minutes for a reply (`A2ACLI_TIMEOUT=5m`,
instead of the CLI's 30s default); set `--env A2ACLI_TIMEOUT=<duration>` to
change it.

Not in this version: an egress proxy and credential injection. The container
can reach the network, and API keys passed with `--secret` are accessible inside it.

## Project MCP servers and skills

The agent gets the MCP servers and skills of its project, as the harness does
when you run it in that directory.

- **MCP servers.** The server reads `.mcp.json` from the workspace and passes
  its servers to the harness over ACP when a conversation starts, so every
  ACP harness gets them. Edits apply to the next conversation.

  ```json
  {"mcpServers": {
    "github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
               "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"}},
    "docs":   {"type": "http", "url": "https://docs.example.com/mcp",
               "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
  }}
  ```

  `${VAR}` and `${VAR:-default}` take their values from the agent's `--secret`
  and `--env` values, so tokens stay out of the file:
  `a2a serve config set --secret GITHUB_TOKEN`. Other secrets, such as bot
  tokens, can't be referenced. A server with a variable that has no value is
  left out; `status` and `doctor` name the variables.
- **Where they run.** Servers that start a command run inside the sandbox, as
  children of the harness. The harness images have `npx`, `python3` and
  `uvx`.
- **Skills.** Skills in `.claude/skills`, `.opencode/skills` and
  `.agents/skills` work with every harness profile. opencode reads all three;
  for Claude Code, the server links the skills of the other two folders into
  `~/.claude/skills` in the harness home when it starts. A skill in
  `.claude/skills` wins over one with the same name elsewhere. With
  `--sandbox local` the harness uses your own home, which the server doesn't
  change, so only the folders the harness reads itself work.
- `a2a serve status` lists the MCP servers and skills the agent found.

Like `config.yaml`, a project's `.mcp.json` decides what runs inside the
sandbox. The harness can edit it, and the change applies to the next
conversation.

Not in this version: limiting a server to the conversations of chosen roles,
adding the tokens of HTTP servers outside the sandbox, and the MCP servers and
skills in your home directory on the host.

## Approvals

The harness is configured so that every tool call that changes something asks
first. A tool call pauses the task in `input-required` with a request of the
[Structured Input extension](https://github.com/a2aproject/experimental-ext-structured-input),
which the agent card advertises: a data part with the JSON Schema of the answer,
an object whose `option` is one of `once`, `always` or `reject`. A `toolCall`
data part describes the call.

Answer with a message on the same task: the option ID or label, `approve`,
`deny`, or structured input `{"option": "once"}` tagged with the request ID
(`structuredinput.NewResponse` in a2a-go).

- The **owner** answers approvals. `--approver pair:anna` adds approvers.
- A **member** can talk to the agent, but can't approve. When a member's task
  needs an approval, the owner gets it on their messengers with buttons, and the
  member sees `input-required` until the owner decides.
- `--allow-tools read,search` and `--deny-tools delete` answer by ACP tool kind
  without asking.
- Every question also has a page, `/hitl/<token>`, linked from email approvals.

## Inspector

`a2a serve open` opens a page where the owner sees what the agent is doing:

- conversations and their tasks, with messages and artifacts, read-only;
- pending approvals, with a button for each option;
- the agent's URLs, task counts, messengers, pairings, webhook nodes and
  contacts, as `a2a serve status` shows them.

The command mints a one-time login link over the control socket, so only
someone with access to the agent directory can open it. The link works once,
for 5 minutes, and the session lasts 12 hours or until the agent restarts.
The page is served on the local URL, and with `open --private` on the private
network; never on the public URL. `--no-browser` prints the link instead of
opening it.

Not in this version: files, live logs, backups, and changing settings from the
page.

## Who can call the agent

| Caller | How it is recognized | Principal |
| --- | --- | --- |
| You, on the machine running the agent | The owner token the server saves in your a2a credentials | `user:owner` |
| Messenger account | Linked once with a pairing code | `user:owner` or `pair:<name>` |
| Your machines on the tailnet | Tailscale `WhoIs`: the same user as the agent | `user:owner` |
| Other tailnet users | `--identity login=role` (`network.identities` in `config.yaml`), or the `a2aproject.org/cap/a2a` app capability | `user:<login>` |
| Anyone else, on any listener | Bearer token from pairing | `pair:<name>` |
| People on the allowlist | Signed in with Google, see [Google sign-in](#google-sign-in) | `google:<email>` |
| [Webhook nodes](#webhook-nodes) | Their node token, as members | `node:<name>` |

```sh
a2a serve pair invite grok                 # member: can talk to the agent
a2a serve pair invite anna --role owner    # owner: can also approve
a2a serve pair invite anna                 # another code: a second device or account
a2a serve pair list                        # the pairings
a2a serve pair get anna                    # role, tokens, linked accounts, pending codes
a2a serve pair set anna --role member      # change the role in place
a2a serve pair set anna --rotate           # revoke her tokens and print a new code
a2a serve pair rm grok                     # revoke its tokens and messenger links
```

A code works once, for 15 minutes. The caller exchanges it with
`a2a serve pair join <agent-url> <code>`, or sends `join <code>` to the agent's bot.
`a2a serve pair list --joined` and `pair rm <agent-url>` manage the agents you joined.
The agent card and `/pair` are public; every A2A request needs a principal.

The owner token the server saves for this machine is listed as `local`.
`a2a serve pair rm local` revokes it, and the next start issues a new one;
the name `local` can't be invited.

## Google sign-in

People can call the agent after signing in with Google, limited to an email
allowlist, with no pairing command:

```sh
a2a serve config set --allow-email mom@example.com,'*@example.com'
a2a serve config set --google-client-id env:GOOGLE_CLIENT_ID \
  --google-client-secret env:GOOGLE_CLIENT_SECRET
a2a send -a https://coder.example.com "hi"      # signs in once
```

- The agent is its own OAuth authorization server and issues its own access
  tokens, which work only at this agent. A request without a token gets a
  `401` whose `WWW-Authenticate` header points to
  `/.well-known/oauth-protected-resource`, and from there to the agent's
  authorization server metadata. A client follows it, signs the user in and
  retries ([a2aproject/a2a-cli#98](https://github.com/a2aproject/a2a-cli/issues/98)).
- With your own Google app (`--google-client-id` and
  `--google-client-secret`), clients use `/oauth/authorize` with PKCE and a
  loopback redirect; Google sends the user back to `<agent-url>/oauth/callback`,
  which must be an authorized redirect URI of the app. Use the URL people call
  the agent at, usually the public one.
- Without your own app, clients sign in with the A2A project's shared app and
  trade the Google ID token at `/oauth/token` (token exchange). The token's
  nonce must be a hash of the agent's URL, so an agent of the same app can't
  reuse a caller's token elsewhere. This works once the shared app exists and
  clients support it.
- The agent checks that the email is verified and on the allowlist.
  `*@example.com` matches only accounts of the example.com Google Workspace:
  anyone can create a personal Google account with an @example.com address.
- People who sign in are members. Removing an email from the allowlist
  (`a2a serve config unset allow-email <entry>`) cuts them off at their next
  request; `a2a serve pair rm google:<email>` revokes their tokens.
- The allowlist belongs to the agent's auth, so it works the same on Funnel,
  ngrok or your own domain.

Not in this version: providers other than Google, and browser sign-in with
the shared app.

## Networks

| Flag | Reachable from | Identity |
| --- | --- | --- |
| *(none)* | localhost and messengers | pairing tokens |
| `--private tailscale` | your tailnet, as `https://<name>.<tailnet>.ts.net` | Tailscale `WhoIs` |
| `--private lan` | the local network, over plain HTTP; peers found with mDNS | pairing tokens |
| `--public tailscale` | the internet through Funnel, at the same URL | pairing tokens |
| `--public ngrok` | the internet, `--ngrok-url` for a reserved domain | pairing tokens |
| `--private https://relay…` | other a2a-serve agents on an [Agent Mesh relay](#agent-mesh-relays-experimental) | the relay's verified identity |
| `--public https://relay…` | the internet, at a URL on the relay | pairing tokens |

Each agent joins the tailnet as its own machine with
[tsnet](https://tailscale.com/kb/1244/tsnet), even when the computer already
runs Tailscale. On the first run it prints a login link, or use
`--tailscale-authkey env:TS_AUTHKEY`. Turn off key expiry for the machine in the
admin console. HTTPS must be enabled in the tailnet, and Funnel needs the
`funnel` node attribute in the tailnet policy. `--tailscale-control-url` points
at Headscale.

### Agent Mesh relays (experimental)

An agent can join an [Agent Mesh](https://github.com/google/agentmesh) relay
and needs no Tailscale account. Agents on the relay call each other from
anywhere, including from behind NAT, and each can get a public URL on the
relay:

```sh
a2a serve run opencode --private https://relay.example.com --public https://relay.example.com
# first run: → sign in to the relay https://relay.example.com: https://…
a2a serve config set --relay-token env:RELAY_TOKEN    # servers: join without the link
```

- Name the relay by its URL; there are no other relay flags. The agent runs
  Agent Mesh's `meshnet` library in-process and keeps its identity in
  `state/agentmesh`, so it keeps its URLs across restarts.
- The first run prints a sign-in link to the relay's identity provider, as
  with Tailscale. `--relay-token` joins with a token instead.
- Agents signed in as the same person treat each other as the owner, as on a
  tailnet. Other agents on the relay are members, recognized like other
  callers of a private network (`--identity <email>=member`). Agents which
  joined with a token don't say who owns them.
- The public URL forwards requests unchanged, so pairing codes and Google
  sign-in work there.
- The private URL (`http://agentmesh.local/mesh/<peer>/a2a/<name>`) works only
  from other a2a-serve agents on the relay; `url` and `status` say so. Give
  people the public URL.
- Source builds need `-tags agentmesh`
  (`go install -tags agentmesh github.com/a2aproject/a2a-serve@latest`). It
  adds about 36 MB to the binary and many Go modules, so release binaries
  leave it out until `meshnet` is a separate Go module. Without it, a relay
  URL fails with that hint.
- This build joins relays with Agent Mesh v0.2.0. `doctor` checks that the
  build supports relays and, with `--online`, that the relay answers; relays
  don't report their version yet.

[Lesson 3](../examples/03-agentmesh-relay/) runs two agents on a relay.

## Messengers

```sh
a2a serve channel add telegram --token env:CODER_BOT_TOKEN
a2a serve channel add discord --token env:CODER_DISCORD_TOKEN
a2a serve channel add email --email-address coder@example.com \
  --email-imap imap.example.com:993 --email-smtp smtp.example.com:465 \
  --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com
a2a serve channel list                                 # and whether they run
a2a serve channel rm discord
```

A bot token can be polled by only one process, so each agent gets its own bot.
On start, the server prints a link for linking yourself as the owner (for
Telegram, a `t.me` deep link). Unknown senders get a pairing hint; their
attachments aren't downloaded. `channel add` checks a bot token before saving
it. A messenger that fails while running is restarted with backoff, and
`a2a serve status` shows its error in the meantime.

- **Telegram:** long polling. Answers are streamed by editing a message about
  once a second; approvals are inline buttons. In groups the bot answers when
  mentioned or replied to.
- **Discord:** gateway WebSocket with the Message Content intent; `/link`,
  `/new` and `/stop` commands; buttons; one thread per conversation in guild
  channels; missed messages are fetched on reconnect.
- **Email:** IMAP IDLE and SMTP. Only mail with `dmarc=pass` for the sender's
  domain or aligned `dkim=pass` in the topmost `Authentication-Results` of the
  trusted server is accepted. Mail with several `From` addresses, auto-replies,
  lists and the agent's own mail are dropped. SMTP needs TLS (port 465 or
  STARTTLS) unless the server is on localhost. Replies stay in the
  thread, and only questions and final results are sent. Approve with a
  one-word reply: `approve`, `always` or `deny`.

A conversation (chat, thread, email thread) is one A2A context. `/new` starts
a new one, and `/stop` cancels the running task.

## Agent to agent

The harness calls contacts and webhook nodes with the `a2a` CLI. The server
proxies the calls, so they use this agent's identity, tailnet and stored
tokens, which never enter the sandbox:

- Every new prompt starts with an `[a2a agents]` line with the agents URL of
  the conversation and the agents the harness can call:
  `[a2a agents] Agents URL for this conversation: http://a2a/k3 (agents: coder, grok; …)`.
  The handle (`k3`) stands for the A2A context, the user the turn is for and
  the hop count, which decide whose behalf calls are made on and where results
  are posted. It isn't a secret: only the harness reaches the socket.
- `<agents URL>/<name>` is an A2A agent, so the harness runs
  `a2a send -a http://a2a/k3/coder "…"`.
- The how-to is in the harness's global instructions, which the server keeps in
  a marked section of `~/.config/opencode/AGENTS.md` and `~/.claude/CLAUDE.md`
  in `state/home`, along with the list of agents. A local harness uses your
  home, so it only gets the `[a2a agents]` line.

```sh
a2a serve peers                     # agents found on the private network
a2a serve contact add coder         # a peer on the tailnet: no code needed
a2a serve contact add grok --url https://grok.example.com --code K7Q2-9XF4
a2a serve contact list
a2a serve contact rm coder
```

- `a2a send` waits for the answer. With `--async` it returns the task ID at
  once, and the server watches the task. When it finishes, its result is
  posted into the originating conversation as a new turn, so the agent
  continues and tells the user.
- Calls carry `X-A2A-Serve-On-Behalf-Of` and a hop count (at most 3), which
  stops agents from calling each other in a loop. The callee counts hops from
  every caller, but honors `X-A2A-Serve-On-Behalf-Of` only from your own agents
  (the same tailnet login), since the user it names belongs to the caller.
- When a called agent needs an approval, the calling agent relays it to the
  user in the chat they are already in: "coder wants to run git push",
  with buttons. The answer goes back to the remote task, and its result is
  posted into the conversation. The harness itself can't answer approvals.

## Schedules

A schedule runs a prompt with no person on the other side: a weekday
briefing, triage every two hours, a reminder on Friday at 5.

```sh
a2a serve schedule add morning-brief --cron "0 7 * * 1-5" --tz Europe/Zurich \
  --prompt "Summarize today's meetings." --deliver telegram:owner
a2a serve schedule add triage --cron "@every 2h" --prompt "Triage new issues." --conversation same
a2a serve schedule add report --at "2026-11-06 17:00" --prompt "Remind me to send the report." --deliver telegram
a2a serve schedule list
a2a serve schedule run morning-brief   # run it now
a2a serve schedule rm morning-brief
```

- Each run is an ordinary A2A task, run as `--as` (the owner by default, or a
  pairing name). `--cron` takes five fields or a descriptor such as `@daily`
  or `@every 2h`; `--at` runs once and then removes the schedule. `--tz`
  defaults to the server's time zone.
- `--deliver <channel>[:<who>]` shows each run in that user's direct chat on
  the channel, with its approval requests. The run becomes the chat's
  conversation, so replying continues it. With `--conversation new` (the
  default) every run starts a new conversation; with `same` it continues the
  chat's current one.
- A run missed while the agent was stopped runs once when it starts.

The harness creates schedules too. When a user asks "remind me on Friday at
5", the harness calls the built-in `schedules` agent at its agents URL
(`a2a send -a http://a2a/<handle>/schedules 'add …'`, as the `a2a-agents`
skill describes). That schedule runs as the user who asked and reports back
in the same conversation. A user's conversation can list and remove only the
schedules they made or run as; the owner sees all of them with
`a2a serve schedule list`. Calls that came through another agent can't
manage schedules.

Not in this version: schedules in `config.yaml`, and editing a schedule in
place (remove it and add it again).

## Webhook nodes

An agent can host agents that only run when woken, such as a Grok Bot
routine. A node is a pairing with a webhook:

```sh
a2a serve pair invite grok --webhook https://<grok routine webhook URL> \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
# → a message for the agent behind the webhook: its skill URL and a pairing code
a2a serve pair invite grok          # a new code, if the node lost its token
a2a serve pair set grok --webhook https://<new URL>   # change the node in place
a2a serve pair set grok --rotate    # revoke its token, change the wake secret, new code
a2a serve pair get grok             # webhook, headers, mailbox counts
a2a serve pair rm grok
```

`pair invite --webhook` for an existing node fails and names `pair set`,
which changes the webhook, headers, description and advertising in place;
the node's token keeps working until `--rotate`. The node reads its mailbox
with `a2a-serve mailbox list`, `mailbox get <task>` and `mailbox reply`.

The agent calls the node as `grok` (`a2a send -a http://a2a/<handle>/grok`), and it is at
`<agent-url>/n/grok` for the owner. The server keeps its tasks in SQLite and
holds them in its mailbox, and
wakes it with a POST signed with
`X-A2A-Serve-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. The node
answers with `a2a-serve mailbox reply <task> --final "…"`. A task with no event
for 10 minutes fails. The node's instructions are served at
`<agent-url>/mailbox/skill.md`.

The node token also authenticates A2A calls to the agent, as the member
`node:grok`, so the node can send the agent tasks of its own:
`a2a send -a <agent-url> "…"` attaches the token saved by `a2a serve pair join`, or
`$A2A_NODE_TOKEN`. `pair rm grok` revokes both directions.

### Advertised pairings

Only the agent that made a pairing can use it. With `--advertised`, every
agent on the private network learns about a webhook node and can call it
through its host:

```sh
a2a serve pair invite grok --webhook https://… --advertised   # on Ben's agent
a2a serve peers                                               # on Anna's agent: lists grok@ben
a2a serve contact add grok@ben                                # no code needed
```

- Each agent keeps a table of advertisements in `state/ads.json`: its own and
  the ones it heard of. Every minute it swaps the names and versions it knows
  with up to three random agents on the private network (`POST /gossip` on
  the private listener), and each downloads the entries the other has newer.
  After a few rounds every agent has every entry, including agents which
  were offline or can't reach the host.
- The host renews its entries every hour, and an entry nobody renews
  disappears after a day, so the entries of a deleted agent go away.
  `pair rm grok` or `pair set grok --advertised=false` marks the entry as
  removed, and the removal spreads the same way.
- Anna's calls go to Ben's agent at `/n/grok`, found by Ben's name on the
  private network, never at an address taken from the advertisement. Ben's
  agent forwards them to the node and keeps the webhook's credentials. It
  treats Anna's agent like any other caller from the private network: Anna's
  login needs a role there (`--identity` or the Tailscale app capability),
  and an advertised node takes members as well as owners.

## Output and exit codes

Management commands take `-o json` or `-o jsonl`. `status -o json` has the same
schema whether the agent runs or not: `running` is always set, and the
fields of the running server, such as `pid` and `local`, only while it runs.
Removals (`stop`, `channel rm`, `contact rm`, `pair rm`)
print `{"removed": true, "kind": …, "name": …}`, and `stop` prints
`{"name", "stopped", "serviceInstalled", "serviceRemoved"}`.

Errors follow the error contract of the
[a2a CLI](https://github.com/a2aproject/a2a-cli) (`specification/SPEC.md`):
with `-o json` they are printed as `{"error": {"code", "message", "hint"}}`.

| Exit | Code | |
| --- | --- | --- |
| 0 | | Success |
| 1 | `A2ACLI_ERR_INTERNAL`, `A2ACLI_ERR_PROTOCOL` | Any other failure |
| 2 | `A2ACLI_ERR_USAGE` | Invalid arguments, flags or settings |
| 3 | `A2ACLI_ERR_IO` | An agent or service couldn't be reached |
| 4 | `A2ACLI_ERR_CARD_INVALID` | An agent card couldn't be used |
| 5 | `A2ACLI_ERR_TIMEOUT` | `--timeout` expired |
| 6 | `A2ASERVE_ERR_NOT_RUNNING` | The command needs the agent server, which isn't running: `a2a serve run --detach` |
| 7 | `A2ASERVE_ERR_NO_AGENT` | No `.a2a` directory here, in a parent directory, or at `--dir` |

## HTTP routes

Served on every listener:

| Route | |
| --- | --- |
| `GET /`, `GET /.well-known/agent-card.json` | the agent card |
| `POST /` | A2A JSON-RPC |
| other paths | A2A HTTP+JSON (REST) |
| `POST /pair` | exchange a pairing code for a caller or node token |
| `/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`, `/oauth/…` | Google sign-in, when `--allow-email` is set |
| `/hitl/<token>` | answer a question in a browser |
| `/inspect/…` | the owner's web inspector (`a2a serve open`); local and private listeners only |
| `/n/<node>/…` | webhook nodes |
| `/mailbox/api/…` | the node API used by `a2a serve mailbox` |
| `GET /skill.md`, `GET /mailbox/skill.md` | skills for agents calling this one (the `a2a skill` of the server's `a2a`), and for nodes |
| `GET /download/a2a`, `GET /download/a2a-serve` | the server's `a2a` and `a2a-serve` binaries, for nodes |

## Backups

An agent's conversations, pairings and harness sessions live in `.a2a/` on
one machine. Backups keep encrypted snapshots of them elsewhere, so the agent
survives the loss of the machine and moves to another host.

```sh
a2a serve config set --backup gs://my-bucket/agents/coder --backup-key env:BACKUP_KEY --backup-interval 1h
a2a serve backup now
a2a serve backup list
a2a serve restore gs://my-bucket/agents/coder --dir ~/src/app --backup-key env:BACKUP_KEY
```

- A snapshot holds a consistent copy of `server.db` (made with SQLite's
  `VACUUM INTO`), `config.yaml`, `state/secrets`, the advertised pairings in
  `state/ads.json`, `state/home` without its caches (`.cache`, `.npm`), and the
  network identity in `state/tsnet` and `state/agentmesh`. The workspace isn't
  included: it is usually in git.
- `--backup` is a directory, `gs://bucket/prefix` or `s3://bucket/prefix`.
  Google Cloud Storage and S3 go through the `gcloud` and `aws` CLIs, which
  find credentials the usual way (`gcloud auth application-default login`,
  `AWS_PROFILE`, instance metadata); `doctor` checks that the CLI is
  installed.
- Snapshots are always encrypted with `--backup-key`, using a key derived with
  scrypt and AES-256-GCM. The key is stored with the agent's secrets; keep a
  copy somewhere other than the backups, since a restore needs it.
- A running agent takes a snapshot every `--backup-interval` (default 24h),
  the first one an interval after the latest stored snapshot. Snapshots older
  than `--backup-retention` (default 7d) are removed, and the latest one is
  always kept. `status` shows the last and the next snapshot.
- `a2a serve restore` recreates the `.a2a` directory from the latest snapshot,
  or `--snapshot <name>`. The network identity is restored only with
  `--take-over`, so the old and the new host don't run as the same tailnet
  machine; stop the agent on the old host first. Without it, the agent joins
  the network as a new machine.

Not in this version: continuous replication (snapshots are periodic, so the
changes since the last one are lost with the machine), and native Cloud
Storage and S3 clients.

## Command reference

| Command | Description |
|---|---|
| `serve run <harness>` | Create the agent with the flags and start it; in an existing agent, apply the flags like `config set`, then start it. `--detach`, `--service`, `--quiet`. |
| `serve run` | Start the agent of the nearest `.a2a/` directory (or `--dir`). Asks a few questions on a first run in a terminal. |
| `serve config [show]` | The effective settings, secrets as names with their state, and changes waiting for a restart. `--defaults`, `--as-command`, `-o json\|yaml`. |
| `serve config set <flags>` / `unset <setting> [<entry>…]` / `edit` | Change settings with the flags of `serve run <harness>`, validated and printed as a diff. A running agent applies channels, the description and approvers at once. `--dry-run`, `--restart`. |
| `serve status` / `logs [-f]` / `stop [--uninstall]` / `restart` | Manage the running agent; `stop --uninstall` also removes its service, `restart` starts it again in the same mode. |
| `serve url [--local\|--private\|--public]` | The agent's URL: public, then private, then local. `a2a send -a "$(a2a serve url)" …` |
| `serve open [--private]` | Open the web inspector with a one-time login link: conversations and tasks, pending approvals with Approve and Deny, messengers, MCP servers and skills, pairings, nodes and contacts. Owner only. |
| `serve pair invite <name> [--role owner]` | Create a pairing and print its one-time code, for a caller or a messenger account; for an existing name, another code for another device or account. The user running the server needs none: the server saves an owner token for its local URL. |
| `serve pair invite <name> --webhook <url>` | Host a webhook node and create its code. |
| `serve pair list` / `get <name>` | List the pairings, or show one: role, tokens, linked accounts, pending codes, and the webhook and mailbox of a node. |
| `serve pair set <name> [--role] [--webhook …] [--rotate]` | Change a pairing in place; `--rotate` revokes its tokens and prints a new code. |
| `serve pair rm <name>` | Revoke a caller, or remove a node. |
| `serve pair join <agent-url> <code>` | On the caller's side: exchange a code for a token, which `a2a` attaches to requests for that agent through the credential helper. Outside an agent directory (or with `--joined`), `pair list` shows the agents you joined, and `pair get` and `pair rm` take their URL. |
| `serve doctor [--fix] [--online]` | Check the `a2a` CLI, Docker and the agent; `--fix` enables the plugin and the credential helper the `a2a` CLI calls (`a2a-serve credential get`, hidden from help) and makes state files private. `-o json`. |
| `serve completion <shell>` | A completion script for `a2a-serve`: commands, harnesses, channel kinds, enum flags, and the contacts and pairings of a running agent. |
| `serve peers` | Agents found on the private network, and the webhook nodes they advertise (`pair invite --advertised`), which `contact add <node>@<agent>` adds without a code. |
| `serve contact add\|list\|rm` | Agents this agent may call with the `a2a` CLI, through the server. |
| `serve channel add\|rm <telegram\|discord\|email>` / `channel list` | Messengers; `list` shows whether they run. |
| `serve schedule add\|list\|run\|rm` | Run a prompt on a cron expression or once, as a user, and show the runs in their chat. The agent creates schedules too, such as for "remind me on Friday at 5". |
| `serve backup now\|list` / `serve restore <dest>` | Encrypted snapshots of the agent's state to a directory, `gs://` or `s3://`, taken every `--backup-interval`; restore recreates the agent on another host. |
| `serve mailbox list\|get\|reply` | Commands a webhook node runs: the agent holds its tasks in a mailbox until it wakes. |
| `serve run echo` / `run proxy <agent-url>` / `run exec '<command>'` | A local test server: echo the message, forward to another agent, or run a shell command per message (stdin is the message, stdout the reply; `--stream`, `--chunk <delim>`). |

## Configuration reference

`config.yaml` holds what the flags set; each key is changed with the flag of
the same name through `a2a serve config set`. Secret values are references
such as `secret:TELEGRAM_BOT_TOKEN`.

```yaml
name: coder
harness: opencode
command: [cloudcode, acp]        # optional: replaces the profile's command
sandbox: docker                  # or local
workspace: /home/u/src/app       # optional: default the directory holding .a2a
mounts: [/home/u/docs:/docs:ro]  # besides the workspace
secrets: [OPENROUTER_API_KEY]     # passed to the harness as environment variables
port: 46757                      # localhost port, chosen on the first run
network:
  private: tailscale
  public: ngrok
  ngrok_url: https://coder.ngrok.app
  identities: {anna@example.com: member}
channels:
  telegram: {token: secret:CODER_BOT_TOKEN}
approvals:
  allow: [read, search]
  approvers: [pair:anna]
limits: {memory: 8g, cpus: "4", pids: 1024}
auth:                            # --allow-email, --google-client-id, --google-client-secret
  allow_emails: [mom@example.com, "*@example.com"]
  google_client_id: 1234.apps.googleusercontent.com
  google_client_secret: secret:GOOGLE_CLIENT_SECRET
backup:                          # --backup, --backup-key, --backup-interval, --backup-retention
  to: gs://my-bucket/agents/coder
  key: secret:BACKUP_KEY
  interval: 1h
  retention: 7d
```

Configs from earlier versions, in which the first of `mounts` is the
workspace, are read as `workspace` and the remaining mounts.

## Test servers

`run echo`, `run proxy` and `run exec` start a local A2A server without an
agent directory, for trying out clients:

```sh
a2a serve run echo                              # reply with the message
a2a serve run proxy https://agent.example.com   # forward to another agent, logging the traffic
a2a serve run exec 'tr a-z A-Z'                 # run a shell command per message
```

All three take `--host`, `--port` (default 8080), `--name`, `--description`,
`--card`, `--card-compat`, `--protocol`, `--transport` (`rest`, `jsonrpc` or
`grpc`), `--advertise-address` and `--quiet`, which stops the traffic log.
`proxy` also takes `--svc-param`, `--auth`, `--tenant` and `--insecure` for
the proxied agent.

`exec` runs the command with `sh -c` for each message, with the message
text on stdin and `A2A_TASK_ID` and `A2A_CONTEXT_ID` in its environment, so
a script can keep state per conversation. Its stdout is the reply artifact;
`--stream` streams it line by line and `--chunk <delim>` in chunks split at
a delimiter. A non-zero exit fails the task with stderr as the message.

## Shell completion

```sh
source <(a2a serve completion bash)    # also zsh, fish, powershell
```

The script completes `a2a-serve` commands, harness names, channel kinds,
enum flags such as `--private`, `--public` and `--sandbox`, and the contacts
and pairings of a running agent. Completing `a2a serve …` through the `a2a`
CLI needs it to forward completion requests to command plugins.

## Testing a harness

`internal/agentsrv/conformance_test.go` runs the conformance scenarios
through the whole server against a scripted ACP agent
(`internal/agentsrv/testdata/fakeacp`): text turn, tool progress, approve,
deny, always-allow, cancel mid-tool, follow-up in context, restart and resume,
agent crash, and a queued concurrent task. To try a real harness without
Docker:

```sh
a2a serve run opencode --sandbox local --command "opencode acp"
```

## What it needs from the a2a CLI

- Command plugins: `a2a-<name>` on `PATH` runs as `a2a <name>`.
- `--unix-socket` (`A2ACLI_UNIX_SOCKET=<path>`): on Linux and with
  `--sandbox local`, the harness's `a2a` sends every request through the
  server's Unix socket.
- Credential helpers (`credential-helper`, the
  [Bazel credential helper protocol](https://github.com/EngFlow/credential-helper-spec)):
  `a2a-serve credential` returns the token `a2a serve pair join` saved in
  `~/.config/a2a-serve/credentials.json` for the longest matching agent URL,
  or `$A2A_NODE_TOKEN` for its agent. `a2a-serve doctor --fix` lists it once
  under `credential-helper` in `~/.config/a2a-cli/config.yaml` as
  `{command: [a2a-serve, credential]}`. On Docker Desktop the harness gets its
  key from a helper script scoped to the bridge host
  (`A2ACLI_CREDENTIAL_HELPER=host.docker.internal=<path>`).
- `a2a skill` and `a2a version -o json`, which the server serves at
  `/skill.md` and `a2a serve doctor` uses to check the installed version.
