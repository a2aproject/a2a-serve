# User guide

This guide covers running and configuring an agent. If you haven't started one yet, begin with [Getting started](getting-started.md). The [command reference](reference/commands/a2a-serve.md) lists every flag.

Each agent lives in its project directory and runs as its own process, with its own workspace, messengers, network identity and pairings. To run several agents on one machine, start one in each directory. They find each other on the private network the same way agents on different machines do.

## Agents and platforms

| Agent | Image |
| --- | --- |
| `opencode` | `ghcr.io/a2aproject/harness-opencode`, with the latest `opencode-ai` at publish time |
| `claude` | `ghcr.io/a2aproject/harness-claude`, with the latest Claude Code and `claude-agent-acp` at publish time |
| `acp --command …` | Any agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com), in any image with `--image` |

`--command` also replaces the command of `opencode` or `claude`, as in `a2a-serve start opencode --command "cloudcode acp"`. The images are Debian with Node.js 22, `git`, `ripgrep`, `python3`, `npx`, `uvx` and the `a2a` CLI.

| Platform | Status |
| --- | --- |
| Linux (amd64, arm64) with Docker, rootful or rootless | Supported. `--service` installs a systemd user unit. |
| macOS (Apple silicon, Intel) with Docker Desktop | Supported. `--service` installs a launchd agent. |
| Linux and macOS without Docker | `--sandbox local` only: the agent runs on the host with no isolation. |
| Windows | Builds, but untested. `--service` isn't available. |

## The agent directory

```
~/src/app/
├── …project files…        # the workspace
└── .a2a/
    ├── config.yaml        # agent settings, no secrets; can be committed
    ├── .gitignore         # "state/"
    └── state/             # never committed
        ├── server.db      # tasks, sessions, tokens, contacts, nodes
        ├── secrets        # tokens and API keys, mode 0600
        ├── tsnet/         # Tailscale identity, so the URL survives restarts
        ├── agentmesh/     # Agent Mesh identity
        ├── home/          # the agent's home directory
        ├── home-anonymous/ # the home of the agent process for anonymous callers
        ├── server.log
        └── server.sock    # control socket for a2a-serve commands
```

`a2a-serve` commands find the agent the way git finds a repository, or take `--dir <path>`. They talk to the running agent over `server.sock`.

Flags that take a secret read the value when you run the command and store it in `state/secrets`, so an agent running in the background doesn't need your shell's environment.

> [!WARNING]
> A project's `config.yaml` and `.mcp.json` decide what runs on your machine. Read them before you start an agent in a repository you didn't write. See [Security](concepts/security.md#what-a-project-can-make-your-machine-run).

## Running

| Command | What it does |
| --- | --- |
| `a2a-serve start` | Starts the agent in the foreground. |
| `a2a-serve start --detach` | Starts it in the background. |
| `a2a-serve start --service` | Installs and starts it as a systemd (Linux) or launchd (macOS) user service. |
| `a2a-serve status` | Shows URLs, messengers, owner links and task counts. |
| `a2a-serve status --url` | Prints only the URL that reaches furthest: public, then private, then local. `--url=local\|private\|public` picks one. |
| `a2a-serve logs [-f]` | Shows the log. |
| `a2a-serve stop [--uninstall]` | Stops it. `--uninstall` also removes the service. |
| `a2a-serve restart` | Stops it and starts it again in the same mode. |

Commands that only change stored data, such as `pair`, `contact add --url`, `contact rm` and `schedule add`, also work while the agent is stopped. It picks up the changes on its next start.

`restart` checks the settings first, so a misconfigured agent keeps running. To restart an agent running in the foreground of another terminal, press Ctrl-C there and run `a2a-serve start` again.

## Changing settings

`a2a-serve config set` takes the same flags as `a2a-serve start opencode`, plus settings that only `config set` takes: container limits, network keys, backups, task retention and Google sign-in. The [config reference](reference/commands/config.md) lists them all.

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

### Secret values

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

## Sandbox

By default the agent runs in Docker:

- The workspace, the directory that holds `.a2a`, is mounted at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config.
- `state/home` is the agent's home directory. [Anonymous callers](#anonymous-callers) get a second container with its own home.
- The container runs as your user, with all capabilities dropped and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Use it only when you trust the agent and everyone who can call it.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

[Security](concepts/security.md#the-sandbox) lists what the sandbox doesn't protect yet.

## Project MCP servers and skills

The agent gets the MCP servers and skills of its project.

- **MCP servers** come from `.mcp.json` in the workspace, read when a conversation starts:

  ```json
  {"mcpServers": {
    "github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
               "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"}},
    "docs":   {"type": "http", "url": "https://docs.example.com/mcp",
               "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
  }}
  ```

  `${VAR}` and `${VAR:-default}` take their values from `--secret` and `--env`, so tokens stay out of the file: `a2a-serve config set --secret GITHUB_TOKEN`. A server with a variable that has no value is skipped, and `status` names the variable. Servers that run a command run inside the sandbox.
- **Skills** in `.claude/skills`, `.opencode/skills` and `.agents/skills` work with both opencode and Claude Code. When two skills share a name, the one in `.claude/skills` wins. With `--sandbox local`, only the folders your agent reads on its own work.

`a2a-serve status` lists the MCP servers and skills the agent found.

Not in this version: MCP servers and skills from your home directory, and limiting a server to some roles.

## Approvals

Every tool call that changes something waits for approval. The task pauses in `input-required` and shows the tool call with three options: `once`, `always` and `reject`. Answer on the same task:

```sh
a2a send --task-id <task-id> approve      # or: once, always, reject, deny
```

Clients can also answer with the [Structured Input extension](https://github.com/a2aproject/experimental-ext-structured-input), which the agent card advertises.

`--allow-tools` and `--deny-tools` answer by tool kind without asking. The kinds are `read`, `edit`, `delete`, `move`, `search`, `execute`, `think`, `fetch`, `switch_mode` and `other`; `*` matches all of them.

The person who started a task answers its approvals, in the place the task started. Owners can answer any approval.

| Task started by | Who answers | Where |
| --- | --- | --- |
| An A2A caller | that caller | in the task |
| A messenger user | that user | the same chat |
| Another agent | the person who asked that agent | their chat, relayed by their agent |
| A webhook node | the node | in the task |
| A schedule with `--chat` | the schedule's `--as` user | that chat |
| A schedule without `--chat` | an owner | the owner chat |
| An [anonymous caller](#anonymous-callers) | an owner, within 10 minutes | the owner chat |

- Set the **owner chat** with `--owner-chat <channel>[:<who>]`. Without it, a2a-serve picks the first messenger with a linked owner. Without one, questions wait in the [inspector](#inspector).
- In a group chat, only the person who started the task, or an owner, can answer.
- Authenticated callers approve their own tasks, so use `--deny-tools` to limit what they can run.
- Email approvals link to a web page for the answer.

## Inspector

`a2a-serve inspect` opens a web page with the agent's conversations, tasks, pending approvals and status. You can answer approvals there.

The login link works once, for 5 minutes, and only someone with access to the agent directory can get one. The page is on the local URL. `inspect --private` opens it on the private URL of a tailnet, which uses HTTPS. The LAN, relays and the public URL don't serve it, because its login link and session would cross the network unencrypted.

Not in this version: files, live logs, backups, and changing settings from the page.

## Who can call the agent

`a2a-serve status` says who can call right now:

```
  access   you: this machine and your tailnet devices
           everyone on the tailnet, emails matching *@example.com, 2 pairings
           the public URL needs a pairing or Google sign-in
```

| Caller | How it is recognized | Role |
| --- | --- | --- |
| You, on the machine running the agent | The token a2a-serve saves for you on start, listed as `local` | owner |
| Your devices on the tailnet | Tailscale identity: the same user as the agent, on an untagged device | owner |
| Everyone else on the private network, unless `--private-access paired` | The tailnet's or relay's identity, or the address on the LAN | authenticated |
| Messenger accounts | Linked with a pairing code | the pairing's role |
| People whose email matches `--allow-email` | A verified email: [Google sign-in](#google-sign-in), a tailnet or relay login, or mail that passes DMARC | authenticated |
| Members of `--owner` and `--allow` | A verified email as above, or everyone on the private network with `--owner private` | owner, authenticated |
| Anyone else | A token from a pairing code | the pairing's role |
| [Webhook nodes](#webhook-nodes) | Their node token | authenticated |
| Unlinked senders in direct chats, with `--public-access anyone` | Their chat account | anonymous |
| Anyone on the public URL, with `--public-access anyone` | Nothing | anonymous |

These settings are flags of `start` and `config set` ([reference](reference/commands/config.md)). A running agent applies them at once, and every change prints who can call afterwards:

```sh
a2a-serve config set --private-access paired           # on the private network, only you and your pairings
a2a-serve config set --allow-email '*@example.com'     # people with a matching verified email
a2a-serve config set --owner email:me@example.com      # another account of yours, as an owner
a2a-serve config set --public-access anyone            # anyone on the public URL or in a direct chat, as an anonymous caller
a2a-serve config unset owner email:me@example.com
```

- A call that comes through another agent gets at most `authenticated`, even when the calling agent is yours.
- On a relay that admits people by sign-in, `--private-access` defaults to `paired` until you set it.
- [Anonymous callers](#anonymous-callers) start tasks in their own conversation and never answer approvals.
- A caller without a credential gets 401, with the steps to pair. A known caller the agent doesn't accept gets 403, naming who they were seen as, such as `anna@example.com on the tailnet`.

The [access model](concepts/access-model.md) describes the model and its guarantees.

```sh
a2a-serve pair invite grok                        # authenticated: can talk to the agent
a2a-serve pair invite anna --owner                # owner: also sees and answers every task
a2a-serve pair invite anna                        # another code, for a second device
a2a-serve pair list                               # kind, role, linked messengers, last use
a2a-serve pair get anna                           # kind, role, tokens, linked accounts, pending codes
a2a-serve pair set anna --owner=false             # change the role
a2a-serve pair set laptop --kind device           # change the kind: person, device or agent
a2a-serve pair set anna --rotate                  # revoke her tokens and print a new code
a2a-serve pair rm grok                            # delete the pairing, its tokens and messenger links
```

A code works once, for 15 minutes. The caller redeems it with `a2a-serve pair join <agent-url> <code>`, or sends `join <code>` to the agent's bot. `a2a-serve pair list --joined` lists the agents you joined, and `pair rm <agent-url>` forgets one.

- The first code redeemed sets the pairing's kind: `pair join` and `join <code>` make a person, and `contact add --code` makes an agent.
- A change of role applies to the next request of every token and messenger account of the pairing.
- `pair rm` says when the person may still call through the private network or an `--allow-email` entry.
- The built-in `local` pairing is you. Its token works only on the local URL, and the link printed at start adds your messenger accounts to it. `pair rm local` revokes both, and the next start issues a new token. The names `local` and `owner` stand for you, so no other pairing can take them.

### Anonymous callers

With `--public-access anyone`, anyone on the public URL, and anyone who writes to the agent's bot in a direct chat, can start tasks. The agent still ignores unlinked senders in group chats.

- Their conversations run in a separate agent process. With Docker it is a second container with its own home directory, `state/home-anonymous`, so it can't read the history of other conversations. It works on the same workspace, and it isn't backed up.
- Their approvals go to the owner chat, or wait in the inspector. Only owners answer them. An approval that nobody answers in 10 minutes is rejected.
- Their conversations can't call other agents or use the schedules agent.
- The public URL and each messenger take at most 20 new anonymous tasks an hour, and run at most 2 at a time.
- A caller on the public URL without a token proves nothing about themselves. The task and conversation IDs that the server returns let them read and continue their own tasks. Anyone who has those IDs can do the same.

## Google sign-in

People on an email allowlist can call the agent with no pairing code. On the public URL they sign in with Google. The allowlist is `--allow-email`, and the emails of `--owner` and `--allow`:

```sh
a2a-serve config set --allow-email mom@example.com,'*@example.com'
a2a-serve config set --google-client-id env:GOOGLE_CLIENT_ID \
  --google-client-secret env:GOOGLE_CLIENT_SECRET
a2a send -a https://coder.example.com "hi"      # signs in once
```

- Add `<agent-url>/oauth/callback` to the authorized redirect URIs of your Google app. Use the URL people call, usually the public one.
- `*@example.com` matches accounts of the example.com Google Workspace, and mail from example.com that passes DMARC. Anyone can create a personal Google account with an @example.com address, so such accounts don't match. Tailnet and relay logins match only full addresses.
- People who sign in get the role their email has: authenticated, or owner with `--owner email:<address>`. Their tokens carry the email, so removing it from the allowlist cuts them off at their next request.
- Only the public URL serves sign-in: Funnel or a relay. On the local URL and the private network, people use pairings or their network identity.

Not in this version: providers other than Google, and sign-in without your own Google app.

## Networks

| Flag | Reachable from | Callers identified by |
| --- | --- | --- |
| *(none)* | localhost and messengers | pairing tokens |
| `--private tailscale` | your tailnet, as `https://<name>.<tailnet>.ts.net` | Tailscale identity |
| `--private lan` | the local network, over plain HTTP | the caller's address |
| `--private <relay-url>` | other a2a-serve agents on an [Agent Mesh relay](#agent-mesh-relays-experimental) | the relay's identity |
| `--public tailscale` | the internet through Funnel, at the same URL | pairing tokens |
| `--public <relay-url>` | the internet, at a URL on the relay | pairing tokens |

### Tailscale

Each agent joins your tailnet as its own machine, even when the computer already runs Tailscale. On the first run it prints a login link; on a headless machine, use `--tailscale-authkey env:TS_AUTHKEY`. Then:

- turn off key expiry for the machine in the admin console;
- enable HTTPS certificates for the tailnet;
- for Funnel, add the `funnel` node attribute to the tailnet policy.

`--tailscale-control-url` points at a Headscale server.

### Agent Mesh relays (experimental)

An agent can join an [Agent Mesh](https://google.github.io/agentmesh/) relay with no Tailscale account. Agents on the relay can call each other from behind NAT, and each can get a public URL:

```sh
a2a-serve start opencode --private https://relay.example.com --public https://relay.example.com
# first run: → sign in to the relay https://relay.example.com: https://…
a2a-serve config set --relay-token env:RELAY_TOKEN    # on servers: join with a token
```

- A relay that admits people by sign-in accepts anyone its identity provider accepts, so on it `--private-access` defaults to `paired`: only you, your pairings and `--allow-email` matches can call. On a relay that agents join only with tokens, everyone on the relay can call.
- The private URL works only from other a2a-serve agents on the relay. Give people the public URL.
- Relays need a build with `-tags agentmesh`: `go install -tags agentmesh github.com/a2aproject/a2a-serve@latest`. Release binaries don't include it yet.

[Lesson 3](../examples/03-agentmesh-relay/README.md) runs two agents on a relay, and [Agent Mesh](concepts/agent-mesh.md) explains how the two projects fit together.

## Messengers

```sh
a2a-serve channel add telegram --token env:CODER_BOT_TOKEN
a2a-serve channel add discord --token env:CODER_DISCORD_TOKEN
a2a-serve channel add email --email-address coder@example.com \
  --email-imap imap.example.com:993 --email-smtp smtp.example.com:465 \
  --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com
a2a-serve channel list
a2a-serve channel rm discord
```

Each agent needs its own bot, because only one process can use a bot token. `channel add` prints a link that makes you the owner of the chat. Unknown senders get a hint on how to pair.

Each chat or thread is one conversation. `/new` starts a new one, and `/stop` cancels the running task.

- **Telegram:** answers stream into one message, and approvals are buttons. In groups, the bot answers when mentioned or replied to.
- **Discord:** needs the Message Content intent. Each conversation gets a thread in server channels. `/join`, `/new` and `/stop` are slash commands.
- **Email:** the agent accepts mail that passes DMARC or aligned DKIM at your trusted mail server (`--email-trusted-authserv`). It replies in the thread, with only questions and final results. Approve by replying `approve`, `always` or `deny`.

## Agents calling agents

The agent calls your contacts and webhook nodes with the `a2a` CLI. The server makes the calls with its own credentials, so tokens never enter the sandbox.

```sh
a2a-serve peers                     # agents on the private network
a2a-serve contact add coder         # a peer on the tailnet: no code needed
a2a-serve contact add grok --url https://grok.example.com --code K7Q2-9XF4
a2a-serve contact list
a2a-serve contact rm coder
```

- Each prompt tells the agent which agents it can call.
- With `a2a send --async`, the agent moves on, and the result arrives later in the same conversation.
- When the called agent asks for approval, the question comes to you in your chat, and your answer goes back to it.
- A chain of calls stops after 3 hops, so agents can't call each other in a loop.

## Schedules

A schedule runs a prompt at set times: a weekday briefing, triage every two hours, a reminder on Friday at 5.

```sh
a2a-serve schedule add morning-brief --cron "0 7 * * 1-5" --tz Europe/Zurich \
  --prompt "Summarize today's meetings." --chat telegram:owner
a2a-serve schedule add triage --cron "@every 2h" --prompt "Triage new issues." --thread same
a2a-serve schedule add report --at "2026-11-06 17:00" --prompt "Remind me to send the report." --chat telegram
a2a-serve schedule list
a2a-serve schedule run morning-brief   # run it now
a2a-serve schedule rm morning-brief
```

- `--cron` takes five fields or a descriptor such as `@daily` or `@every 2h`. `--at` runs once and then removes the schedule. `--tz` defaults to the machine's time zone.
- `--as` runs the schedule as a pairing; the default is the owner.
- `--chat <channel>[:<who>]` posts each run to that user's chat, where they can reply and answer approvals. `--thread new` (the default) starts a new conversation each run; `--thread same` continues the current one. Without `--chat`, approvals go to the owner chat.
- A run missed while the agent was stopped runs once on the next start.

The agent can create schedules too. When someone asks "remind me on Friday at 5", the agent adds a schedule that runs as that person and reports back in the same conversation. People can list and remove only their own schedules; `a2a-serve schedule list` shows all of them.

Not in this version: schedules in `config.yaml`, and editing a schedule. Remove it and add it again.

## Webhook nodes

A webhook node is an agent that runs only when woken, such as a Grok bot routine. The server gives it an A2A address, holds its tasks and wakes it with a signed POST to its webhook:

```sh
a2a-serve pair invite grok --webhook https://<grok routine webhook URL> \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
# → a message for the agent behind the webhook: its instructions and a pairing code
a2a-serve pair invite grok          # a new code, if the node lost its token
a2a-serve pair set grok --webhook https://<new URL>
a2a-serve pair set grok --rotate    # revoke its token and print a new code
a2a-serve pair get grok             # webhook, headers, pending tasks
a2a-serve pair rm grok
```

The node reads its instructions at `<agent-url>/mailbox/skill.md` and answers with `a2a-serve mailbox reply <task> --final "…"`. A task with no reply for 10 minutes fails.

Your agent calls the node as `grok`. You can call it at `<agent-url>/n/grok`. The node can also call your agent with its token, as an authenticated caller.

### Advertised pairings

With `--advertised`, every agent on the private network can find a webhook node and call it through its host:

```sh
a2a-serve pair invite grok --webhook https://… --advertised   # on Ben's agent
a2a-serve peers                                               # on Anna's agent: lists grok@ben
a2a-serve contact add grok@ben                                # no code needed
```

Anna's calls go through Ben's agent, which keeps the webhook's credentials. Anna's login needs a role on Ben's agent, through `--identity` or the Tailscale app capability. An advertisement spreads to all agents within a few minutes; removing the node or passing `--advertised=false` withdraws it the same way.

## Backups

Backups keep encrypted snapshots of an agent's conversations, pairings, secrets and network identity, so you can restore it on another machine.

```sh
a2a-serve config set --backup gs://my-bucket/agents/coder --backup-key env:BACKUP_KEY --backup-interval 1h
a2a-serve backup now
a2a-serve backup list
a2a-serve backup restore gs://my-bucket/agents/coder --dir ~/src/app --backup-key env:BACKUP_KEY
```

- `--backup` is a directory, `gs://bucket/prefix` or `s3://bucket/prefix`. Cloud Storage and S3 use the `gcloud` and `aws` CLIs and their usual credentials.
- Keep a copy of `--backup-key` somewhere other than the backups. A restore needs it.
- A running agent takes a snapshot every `--backup-interval` (default 24h) and removes snapshots older than `--backup-retention` (default 7d). The latest one is always kept.
- Snapshots don't include the workspace, which is usually in git.
- `backup restore` restores the latest snapshot, or the one named with `--snapshot`. Add `--take-over` to keep the agent's network identity and URLs; stop the agent on the old machine first.

Not in this version: continuous replication. Changes since the last snapshot are lost with the machine.

## Test servers

`start echo`, `start proxy` and `start exec` run a local A2A server with no agent directory, for trying out clients:

```sh
a2a-serve start echo                              # replies with the message
a2a-serve start proxy https://agent.example.com   # forwards to another agent and logs the traffic
a2a-serve start exec 'tr a-z A-Z'                 # runs a shell command per message
```

`exec` passes the message on stdin, and `A2A_TASK_ID` and `A2A_CONTEXT_ID` in the environment. Its stdout is the reply; a non-zero exit fails the task with stderr as the message.

## Shell completion

```sh
source <(a2a-serve completion bash)    # also zsh, fish, powershell
```
