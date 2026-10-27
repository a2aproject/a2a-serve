# Agent server: `a2a serve <harness>`

> [!WARNING]
> Alpha. Commands, flags and stored state may change.

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

## Quick start

```sh
cd ~/src/app
a2a serve opencode --name coder --secret OPENROUTER_API_KEY
```

The first run saves the flags to `.a2a/config.yaml` and starts the agent in
the foreground. Later runs, from the project or a subdirectory, are just
`a2a serve`. Run `a2a serve` without flags in a new directory and it asks
a few questions instead. Change settings later with the same flags, through
`a2a serve config set` (see [Changing settings](#changing-settings)).

On start, the server saves an owner token for its local URL in your
`~/.config/a2a-serve/credentials.json` and prints how to call it. The `a2a` CLI attaches it through a2a-serve's credential helper, which you
enable once in `~/.config/a2a-cli/config.yaml`:

```yaml
credential-helper: a2a-serve credential
```

Then, from another terminal on the same machine:

```sh
a2a send -a "$(a2a serve url --local)" "What does this project do?"
```

Anyone else needs an invitation:

```sh
a2a serve pair invite anna                    # in ~/src/app: prints a one-time code
a2a serve pair add <agent-url> <code>                # on anna's side: saves a token for that URL
```

`a2a send`, `a2a task …` and `a2a card get` attach the saved token
automatically.

Harnesses: `opencode` (`opencode acp`) and `claude` (Zed's
`claude-agent-acp`). `a2a serve acp --command "<cmd>"` runs any agent that
speaks the [Agent Client Protocol](https://agentclientprotocol.com), and
`--command` also replaces a profile's command, such as
`a2a serve opencode --command "cloudcode acp"`.

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
`--dir <path>`. They talk to the running process over `server.sock`, so
changes apply without a restart.

`--secret VAR`, `--telegram env:VAR` and similar flags read values from your
shell when you run the command and store them in `state/secrets`. A detached
or service process doesn't depend on your shell's environment.

> [!WARNING]
> `config.yaml` decides what runs on your machine: `sandbox: local`,
> `command`, `image`, `mounts`, `env` and `secrets`. Running `a2a serve` in a
> cloned repository runs whatever its `config.yaml` says, with access to the
> mounts and secrets it names. Read a `config.yaml` you didn't write before you
> start an agent from it, as you would a Makefile or a CI config.

## Running it

| Command | What it does |
| --- | --- |
| `a2a serve` | Starts the agent in the foreground. |
| `a2a serve --detach` | Starts it in the background. |
| `a2a serve --service` | Installs and starts a systemd (Linux) or launchd (macOS) user service. |
| `a2a serve status` | URLs, channels, owner links, task counts. |
| `a2a serve url [--local\|--private\|--public]` | The agent's URL which reaches furthest: public, then private, then local. |
| `a2a serve logs [-f]` | The server and harness log. |
| `a2a serve stop [--uninstall]` | Stops it, detached or as a service; `--uninstall` also removes the service. |
| `a2a serve restart` | Stops it and starts it again in the same mode, detached or as a service. |

`restart` checks the settings first, so an agent which couldn't start again
isn't stopped. An agent running in the foreground of another terminal can't
be restarted from here: press Ctrl-C there and run `a2a serve` again.

## Changing settings

`a2a serve config set` takes the flags of `a2a serve <harness>`, so the first
run teaches the vocabulary for later changes. Every change is validated
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
| `a2a serve config show [--defaults] [--as-command]` | The effective settings, with secrets as names and their state (`stored`, `missing`), and the settings changed since the running server started. `--defaults` also lists unset settings; `--as-command` prints the `a2a serve <harness> …` command which recreates the agent, with secrets as `env:NAME`. `-o json` and `-o yaml` for scripts. |
| `a2a serve config set <flags> [--dry-run] [--restart]` | Validates, saves and prints the diff. `--dry-run` prints the diff only; `--restart` restarts a running agent when a change needs it. |
| `a2a serve config unset <setting> [<entry>…]` | Removes entries from a list setting, the whole list without entries, or resets a setting to its default. `<setting>` is the flag name without `--`; `email` removes the email channel. |
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

`unset secret NAME`, and unsetting a channel, also delete the value from
`state/secrets`. `--secret NAME` reads `$NAME`, or prompts for it on a
terminal when it isn't set. The secret-valued flags (`--telegram`,
`--discord`, `--email-password`, `--tailscale-authkey`, `--ngrok-authtoken`)
take `-`: on a terminal it prompts without echo, otherwise it reads stdin,
which keeps tokens out of the shell history:

```sh
a2a serve config set --telegram -
pass show coder/bot | a2a serve config set --discord -
```

A running agent applies channels, the description, approvers and the
allowed and denied tools at once, through its control socket; other settings are saved and wait for
`a2a serve restart`, and `config show` and `status` list them.

`a2a serve <harness>` in an existing agent goes through the same path, then
starts the agent unless it already runs. It refuses another harness:
`this agent runs opencode; switch with a2a serve config set --harness claude`.

An invalid `config.yaml` written by hand stops `a2a serve` with an error
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
[Agent to agent](#agent-to-agent)), over a Unix socket that only the harness
reaches: in Docker on Linux it is mounted at `/run/a2a/a2a.sock`, along with
the `a2a` binary from the server's `PATH` at `/usr/local/bin/a2a`. `A2ACLI_UNIX_SOCKET=a2a=<socket>`
sends requests for the host `a2a` through it, and everything else as usual.
Docker Desktop (macOS) can't mount it, so there the harness connects over TCP
with a key. The server writes a credential helper that returns the key into
the harness home and sets `A2ACLI_CREDENTIAL_HELPER=host.docker.internal=<helper>`,
so the harness's `a2a` sends the key to that host only. The harness images also install `a2a`, at the version of the server's
`a2a`, for macOS; a custom `--image` needs it too. With `--sandbox local` the
directory of the server's `a2a` is put first on the harness's `PATH`. The harness's `a2a` waits up to 5 minutes for a reply
(`A2ACLI_TIMEOUT=5m`, instead of the CLI's 30s default); set
`--env A2ACLI_TIMEOUT=<duration>` to change it.

Not in this version: an egress proxy and credential injection. The container
can reach the network, and API keys passed with `--secret` are visible inside it.

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

## Who can call the agent

| Caller | How it is recognized | Principal |
| --- | --- | --- |
| You, on the machine running the agent | The owner token the server saves in your a2a credentials | `user:owner` |
| Messenger account | Linked once with a pairing code | `user:owner` or `pair:<name>` |
| Your machines on the tailnet | Tailscale `WhoIs`: the same user as the agent | `user:owner` |
| Other tailnet users | `--identity login=role` (`network.identities` in `config.yaml`), or the `a2aproject.org/cap/a2a` app capability | `user:<login>` |
| Anyone else, on any listener | Bearer token from pairing | `pair:<name>` |
| [Webhook nodes](#webhook-nodes) | Their node token, as members | `node:<name>` |

```sh
a2a serve pair invite grok                 # member: can talk to the agent
a2a serve pair invite anna --role owner    # owner: can also approve
a2a serve pair list                        # list issued tokens
a2a serve pair rm grok                     # revoke its tokens and messenger links
```

A code works once, for 15 minutes. The caller exchanges it with
`a2a serve pair add <agent-url> <code>`, or sends `link <code>` to the agent's bot.
The agent card and `/pair` are public; every A2A request needs a principal.

The owner token the server saves for this machine is listed as `local`.
`a2a serve pair rm local` revokes it, and the next start issues a new one;
the name `local` can't be invited.

## Networks

| Flag | Reachable from | Identity |
| --- | --- | --- |
| *(none)* | localhost and messengers | pairing tokens |
| `--private tailscale` | your tailnet, as `https://<name>.<tailnet>.ts.net` | Tailscale `WhoIs` |
| `--private lan` | the local network, over plain HTTP; peers found with mDNS | pairing tokens |
| `--public tailscale` | the internet through Funnel, at the same URL | pairing tokens |
| `--public ngrok` | the internet, `--ngrok-url` for a reserved domain | pairing tokens |

Each agent joins the tailnet as its own machine with
[tsnet](https://tailscale.com/kb/1244/tsnet), even when the computer already
runs Tailscale. On the first run it prints a login link, or use
`--tailscale-authkey env:TS_AUTHKEY`. Turn off key expiry for the machine in the
admin console. HTTPS must be enabled in the tailnet, and Funnel needs the
`funnel` node attribute in the tailnet policy. `--tailscale-control-url` points
at Headscale.

## Messengers

```sh
a2a serve opencode --telegram env:CODER_BOT_TOKEN      # or later:
a2a serve channel add telegram --token env:CODER_BOT_TOKEN
a2a serve channel add discord --token env:CODER_DISCORD_TOKEN
a2a serve channel add email --address coder@example.com \
  --imap imap.example.com:993 --smtp smtp.example.com:465 \
  --password env:MAIL_PASSWORD --trusted-authserv mx.example.com
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

## Webhook nodes

An agent can host agents that only run when woken, such as a Grok Bot
routine. A node is a pairing with a webhook:

```sh
a2a serve pair invite grok --webhook https://<grok routine webhook URL> \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
# → a message for the agent behind the webhook: its skill URL and a pairing code
a2a serve pair invite grok          # a new code, if the node lost its token
a2a serve pair list                 # callers and nodes
a2a serve pair rm grok
```

Running `pair invite --webhook` again for an existing node updates it, and its
previous token stops working.

The agent calls the node as `grok` (`a2a send -a http://a2a/<handle>/grok`), and it is at
`<agent-url>/n/grok` for the owner. The server keeps its tasks in SQLite and
wakes it with a POST signed with
`X-A2A-Serve-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. The node
answers with `a2a-serve relay reply <task> --final "…"`. A task with no event
for 10 minutes fails. The node's instructions are served at
`<agent-url>/relay/skill.md`.

The node token also authenticates A2A calls to the agent, as the member
`node:grok`, so the node can send the agent tasks of its own:
`a2a send -a <agent-url> "…"` attaches the token saved by `a2a serve pair add`, or
`$A2A_RELAY_TOKEN`. `pair rm grok` revokes both directions.

## Output and exit codes

Management commands take `-o json` or `-o jsonl`. `status -o json` has the same
schema whether the agent runs or not: `running` is always set, and the
fields of the running server, such as `pid` and `local`, only while it runs.
Removals (`stop`, `channel rm`, `contact rm`, `pair rm`, `pair forget`)
print `{"removed": true, "kind": …, "name": …}`, and `stop` prints
`{"name", "stopped", "serviceInstalled", "serviceRemoved"}`.

Errors follow the error contract of the [a2a CLI](https://github.com/a2aproject/a2a-cli)
(`specification/SPEC.md`): with `-o json` they are printed as `{"error": {"code", "message", "hint"}}`.

| Exit | Code | |
| --- | --- | --- |
| 0 | | Success |
| 1 | `A2ACLI_ERR_INTERNAL`, `A2ACLI_ERR_PROTOCOL` | Any other failure |
| 2 | `A2ACLI_ERR_USAGE` | Invalid arguments, flags or settings |
| 3 | `A2ACLI_ERR_IO` | An agent or service couldn't be reached |
| 4 | `A2ACLI_ERR_CARD_INVALID` | An agent card couldn't be used |
| 5 | `A2ACLI_ERR_TIMEOUT` | `--timeout` expired |
| 6 | `A2ASERVE_ERR_NOT_RUNNING` | The command needs the agent server, which isn't running: `a2a serve --detach` |
| 7 | `A2ASERVE_ERR_NO_AGENT` | No `.a2a` directory here, in a parent directory, or at `--dir` |

## HTTP routes

Served on every listener:

| Route | |
| --- | --- |
| `GET /`, `GET /.well-known/agent-card.json` | the agent card |
| `POST /` | A2A JSON-RPC |
| other paths | A2A HTTP+JSON (REST) |
| `POST /pair` | exchange a pairing code for a caller or node token |
| `/hitl/<token>` | answer a question in a browser |
| `/n/<node>/…` | webhook nodes |
| `/relay/api/…` | the node API used by `a2a serve relay` |
| `GET /skill.md`, `GET /relay/skill.md` | skills for agents calling this one (the `a2a skill` of the server's `a2a`), and for nodes |
| `GET /download/a2a`, `GET /download/a2a-serve` | the server's `a2a` and `a2a-serve` binaries, for nodes |

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
```

Configs from earlier versions, in which the first of `mounts` is the
workspace, are read as `workspace` and the remaining mounts.

## Testing a harness

`internal/agentsrv/conformance_test.go` runs the conformance scenarios
through the whole server against a scripted ACP agent
(`internal/agentsrv/testdata/fakeacp`): text turn, tool progress, approve,
deny, always-allow, cancel mid-tool, follow-up in context, restart and resume,
agent crash, and a queued concurrent task. To try a real harness without
Docker:

```sh
a2a serve opencode --sandbox local --command "opencode acp"
```
