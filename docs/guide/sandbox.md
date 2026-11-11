# Sandbox

The agent runs in a Docker container that sees your project and nothing else on your disk, and reaches the network through the server.

## What the agent sees

- The workspace, the directory that holds `.a2a`, at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config. The same goes for the `.a2a` directories of other agents in the workspace.
- The agent's own settings files in the workspace, such as `opencode.json` and `.claude/settings.json`, appear empty, so a caller can't change the agent's permissions. Put settings such as a model provider in the agent home directory, `.a2a/state/home`, instead.
- The agent runs as your user, with memory, CPU and process limits that `--memory`, `--cpus` and `--pids` change.

The server refuses a workspace or mount that is your home directory or `/`, because the agent could read your credentials there. Use a project directory.

The image comes from `ghcr.io/a2aproject`. `--image` uses another one.

`--sandbox local` runs the agent on the host, as you, with no isolation. Use it only when you trust the agent and everyone who can call it.

## What each caller's container gets

You and your owner pairings share one container. Each [member](identities.md#roles) gets a container of their own, and [guests](identities.md#guests) share one more. By default:

- your container and the containers of members get the shared workspace and every `--secret` value;
- the container of guests gets a read-only workspace and no secrets.

`a2a-serve status` shows what each kind of caller gets:

```
  sandbox  you: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           emails matching *@example.com: own copy of the workspace, secrets ANTHROPIC_API_KEY
           other members: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           guests: read-only workspace, no secrets
```

### Set what a pairing gets

```sh
a2a-serve pair invite ben --storage read-only --secrets none
a2a-serve pair set ben --storage own --secrets ANTHROPIC_API_KEY
```

`--secrets` takes names from the agent's [`--secret`](secrets.md) list, `all` or `none`.

### Give guests a key of their own

Guests get no secrets, so the agent can't reach a model that needs a key for them. Store a separate key that you can revoke, and pass it to the guests' agent under the name it expects:

```sh
a2a-serve config set --secret GUEST_ANTHROPIC_KEY --guest-secrets ANTHROPIC_API_KEY=secret:GUEST_ANTHROPIC_KEY --restart
```

A pairing gets a key of its own the same way, with `a2a-serve pair set <name> --secrets ANTHROPIC_API_KEY=secret:<STORED_KEY>`.

### Rules for several callers

To set what a group of callers gets, such as everyone with an email of your company, edit the `policy.sandbox` list with `a2a-serve config edit`. The [configuration reference](../reference/configuration.md#sandbox-rules) describes the rules.

### Storage modes

| Mode | Workspace | Mounts |
| --- | --- | --- |
| `shared` | The workspace, as you mounted it. | As configured. |
| `read-only` | The workspace, read-only. | Read-only. |
| `own` | A private copy of the workspace. | Read-only. |
| `empty` | An empty directory. | None. |

An `own` copy is made when the caller's container first starts. Nothing is copied back, so the caller gets their work back in the agent's replies. Copies have a [size limit](../reference/limits.md#sandbox).

### When a change applies

The running agent applies rule changes at once. A change that gives a caller less stops their running tasks. Your own container applies a change at `a2a-serve restart`, and `status` lists the change as pending until then.

## Network

The agent reaches the public internet. Private destinations are blocked: the tailnet, the LAN, this machine and the cloud metadata services. `host.docker.internal` reaches the ports that other containers publish, such as with `-p 5432:5432`.

### Allow a private host

| To reach | Run |
| --- | --- |
| A NAS or another LAN host | `a2a-serve config set --egress-allow nas.lan:22` |
| A model server on another machine | `a2a-serve config set --egress-allow 192.168.1.30:11434` |
| A model server on this machine | `a2a-serve config set --egress-allow host.docker.internal:11434`, and point the agent at `host.docker.internal:11434` |
| A tailnet host | `a2a-serve config set --egress-allow nas.example.ts.net` |

A rule is a host, `*.corp.example.com`, an IP address or a range such as `192.168.1.0/24`, with an optional port. The agent keeps running and the next connection uses the rule. `a2a-serve config unset egress-allow nas.lan:22` removes it.

### Allow only some hosts

```sh
a2a-serve config set --egress allowlist \
  --egress-allow generativelanguage.googleapis.com:443 \
  --egress-allow github.com:443 --restart
```

The agent then reaches only the hosts you list. Include the API of its model provider:

| Agent or provider | Hosts |
| --- | --- |
| opencode | `models.opencode.ai:443`, `registry.npmjs.org:443`, and the API of its provider |
| Claude Code | `api.anthropic.com:443` |
| Google (Gemini) | `generativelanguage.googleapis.com:443` |
| OpenAI | `api.openai.com:443` |
| Anthropic | `api.anthropic.com:443` |
| OpenRouter | `openrouter.ai:443` |
| xAI | `api.x.ai:443` |
| Groq | `api.groq.com:443` |
| Mistral | `api.mistral.ai:443` |
| DeepSeek | `api.deepseek.com:443` |

`a2a-serve config unset egress --restart` goes back to the internet.

### See what was blocked

`a2a-serve status` shows the latest blocked connections and the rule that allows one:

```console
$ a2a-serve status
...
  egress   the internet and nas.lan:22; other private networks blocked
           ! 3 connection(s) blocked since the server started, latest 192.168.1.10:22, nas.lan:443 (allowed, retry), nsa.lan:443 (doesn't resolve)
             allow one with a2a-serve config set --egress-allow 192.168.1.10:22
```

`a2a-serve logs | grep egress` lists each one.

The sandbox has TCP only, apart from DNS. For tools that need UDP, `--egress unrestricted` gives the sandbox Docker's own network, with the LAN and the tailnet.

### Where it applies

Network rules work with Docker, rootless Docker and Docker Desktop. Where the server can't route the sandbox's connections, such as with some Podman setups, the sandbox keeps Docker's own network, and `start`, `status` and `doctor` say why. `--egress allowlist` then doesn't start the agent. `--sandbox local` has no network rules.

[Security](../advanced/security.md#the-sandbox) lists what the sandbox doesn't protect yet.
