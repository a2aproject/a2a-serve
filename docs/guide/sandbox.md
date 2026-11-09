# Sandbox

By default the agent runs in Docker:

- The workspace, the directory that holds `.a2a`, is mounted at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config.
- The `.a2a` directories of other agents in the workspace and the mounts also appear empty, so the agent can't read their secrets or reach their control sockets. The server searches the mounts before it starts each container, and the start output lists the agents it found. It refuses a mount with more than 500,000 directories, which would take too long to search.
- `a2a-serve start` refuses to create an agent inside the directory of a running agent. Stop that agent first; its next start hides the new agent. When an agent appears in the mounts of a running agent in another way, such as through `--mount`, the server refuses to start more containers for the running agent until you run `a2a-serve restart`.
- The server refuses a workspace or a mount that is your home directory, `/` or a directory that holds your home directory, because the agent could read your credentials and the secrets of every agent in it. Use a project directory. `--allow-broad-mount <path>` mounts that path anyway, and the agents in it stay visible to the agent.
- The server checks the mount sources, `.a2a` and the agent home directory before it starts each container. It refuses a path that goes through a symlink inside a writable mount, because the agent can repoint that symlink. The error names the resolved path to configure.
- The server also remembers the agent directory, `.a2a`, `state` and each agent home directory when it first uses them. When the agent renames one of them through a writable mount and creates another in its place, the server refuses to start a container until you check the directory and restart the server.
- `state/home` is the agent's home directory. Each [member](identities.md#roles) gets a container with a home of their own, and [guests](identities.md#guests) share one more. [Sandbox rules](#what-each-callers-container-gets) set how much of the workspace and which secrets those containers get.
- The agent runs as your user, with no capabilities and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`). With rootless Docker it runs as container root, which is your user on the host.
- The agent has no network of its own. Its connections go through the server, which decides each one by its destination ([Network](#network)).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Every conversation can read the others, and the agent can read the `.a2a` directories of every agent on the machine, its own included. The server only warns when the workspace holds other agents. Use it only when you trust the agent and everyone who can call it. On Linux, the server kills the processes that an agent process started, such as tool commands, once that agent process exits.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

## What each caller's container gets

Each caller's conversations run in an agent container. You and your owner pairings share yours, guests share one, and each member gets one of their own. [Identities](identities.md#roles) defines the roles.

Without sandbox rules:

- your container and the containers of members get the shared workspace and every `--secret` value;
- the container of guests gets a read-only workspace and no secrets.

Sandbox rules set what chosen callers get, such as a read-only workspace for a pairing, or a key of their own for guests. The `members` line, or `other members` after rule lines, covers the members that no rule matches. They get the default.

`a2a-serve status` shows what each kind of caller gets:

```
  sandbox  you: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           emails matching *@example.com: own copy of the workspace, secrets ANTHROPIC_API_KEY
           other members: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           guests: read-only workspace, no secrets
```

These lines appear when sandbox rules exist or guests may call; without them, every caller gets the same. `a2a-serve start` shows them when others may call the agent. `config set`, `config edit` and the `pair` commands print them as "what their agent containers get:" after "who may call:". `pair get` shows what the pairing's container gets, and `pair invite` shows it when rules change it. When the agent's model needs a key that a caller's container doesn't get, these commands warn that the agent can't reach its model for them and say how to give one.

### Share the agent

Invite someone, with no extra flags:

```sh
a2a-serve pair invite anna
```

Anna's agent container gets the defaults above. No rule is needed.

### Set what a pairing gets

```sh
a2a-serve pair invite ben --storage read-only --secrets none
a2a-serve pair set ben --storage own --secrets ANTHROPIC_API_KEY
```

`pair invite` and `pair set` add or replace the sandbox rule of the pairing. A flag you leave out keeps that part of the rule. A part the rule leaves out comes from a broader rule that matches the pairing, such as a rule of `private`, or from the default. A pairing that becomes an owner loses its rule. `--secrets` takes names from the agent's [`--secret`](secrets.md) list, `all` or `none`. `pair rm ben` removes the rule with the pairing. Owner pairings refuse `--storage` and `--secrets`, because owners share your container. The [`self` rule](../reference/configuration.md#sandbox-rules) in `config.yaml` sets what that container gets.

### Give guests a key of their own

Guests get no secrets, so the agent can't reach a model that needs a key for them, and `start` and `status` say so. A caller can read a key their container gets, so give them a key of their own that you can revoke. Store it and pass it to the agent under the name it expects, in one command:

```sh
a2a-serve config set --secret GUEST_ANTHROPIC_KEY --guest-secrets ANTHROPIC_API_KEY=secret:GUEST_ANTHROPIC_KEY --restart
```

The agent sees `ANTHROPIC_API_KEY` in the container of guests, with the value of `GUEST_ANTHROPIC_KEY`, and no other secrets. Your container and the other containers that get every `--secret` value also get `GUEST_ANTHROPIC_KEY`. Those callers already get your own key. A new secret loads at restart, so the rule waits for it, and `status` lists it as pending until then.

To share your own key instead, which takes effect at once:

```sh
a2a-serve config set --guest-secrets ANTHROPIC_API_KEY
```

`--guest-secrets` replaces the list each time, and `config unset guest-secrets` resets it. A pairing gets a key of its own the same way, with `a2a-serve pair set <name> --secrets ANTHROPIC_API_KEY=secret:<STORED_KEY>`. When you stored the key since the agent started, the pairing gets it after `a2a-serve restart`. Until then, `pair set` and `pair get` show what the pairing gets after the restart.

### Rules for several callers

`--guest-storage` and `--guest-secrets` set the rule of guests, `guests` in `config.yaml`, and `pair` sets the rule of one pairing. Write the rest, such as a rule for a whole email domain, in `a2a-serve config edit`, in the `policy.sandbox` list that the [configuration reference](../reference/configuration.md#sandbox-rules) describes. `a2a-serve config unset sandbox-rules` removes every rule except the one of guests, including the rules of pairings and of `self`.

### Storage modes

| Mode | Workspace | Mounts |
| --- | --- | --- |
| `shared` | The workspace, as you mounted it. | As configured, writable unless `:ro`. |
| `read-only` | The workspace, read-only. | Read-only. |
| `own` | A private copy of the workspace. | Read-only. |
| `empty` | An empty private directory. | None. |

In every mode, the agent home directory and `/tmp` stay writable.

An `own` copy is made once, when the caller's container first starts. Nothing is copied back to your workspace, so the caller gets work back in the agent's replies. The copy:

- leaves out every `.a2a` directory;
- holds at most 1 GiB in at most 200,000 entries. When the workspace is larger, the container fails to start with an error that suggests `read-only`;
- lives in `.a2a/state/workspaces/`, and [backups](backups.md) leave it out;
- is deleted by `pair rm`, and after 30 days without use for callers without a pairing. `pair rm --dry-run` lists the copy and the rule it would delete. The copies of guests and of your container are deleted once their rule no longer gives them `own` storage.

The server keeps at most 100 copies. A caller whose copy would be the 101st gets an error that suggests `read-only` storage.

The agent's instructions name its storage mode and say how to return work in it.

Storage rules need the Docker sandbox. With `--sandbox local`, every caller's agent runs on your machine as you: rules still keep secrets out of its environment, but it can read them in `.a2a/state/secrets`, and `status` says so.

### When a change applies

A running agent applies the changes of the rules made with `config set`, `config edit` and the `pair` commands at once. A grant of a secret stored since the agent started waits for the restart that loads the secret, and the rest of the change applies at once. A change you make to `config.yaml` in another editor applies at `a2a-serve restart`, and `status` lists it as pending until then.

A change that gives a caller less ends the tasks in their container and stops it, so that they can't keep the access they had by keeping a task waiting. Their next message starts the container with the new rule. A secret the agent already saved in its home stays there, so rotate a secret you take away. Another change reaches a container when its current tasks end. Until then, it takes only answers to the tasks that wait in it, and other messages get an error that asks the caller to send them again. Your own container, which the `self` rule covers, applies a change at `a2a-serve restart`, and `status` notes a change waiting for it.

A conversation runs in the container of the user who started it. When someone else sends a message in it and their rule gives less than that container does, the server refuses the message and suggests that they start a conversation of their own.

`${VAR}` in `.mcp.json` takes the `--env` values and the secrets that the conversation's container gets. An MCP server that needs a variable the container doesn't get is left out of that conversation ([MCP and Skills](mcp-and-skills.md)). A container with `own` storage gets the servers of the `.mcp.json` in its copy, and a container with `empty` storage gets none.

[Security](../advanced/security.md#the-sandbox) lists what the sandbox doesn't protect yet.
## Network

In Docker, every connection the agent opens goes through the server ([where it applies](#where-it-applies)). By default:

- The agent reaches the public internet.
- Private destinations are blocked: the tailnet, the LAN, link-local and unique local addresses, loopback, the addresses of this machine, and every network this machine reaches through another route than the internet, such as a split-tunnel VPN or a bridge. Behind a full-tunnel VPN or a Tailscale exit node, the internet is still the internet.
- `host.docker.internal` reaches the ports that other containers publish on all addresses of this machine, such as with `-p 5432:5432`. With Docker Desktop, it also reaches the ports published on `127.0.0.1`. `--egress-allow host.docker.internal:11434` opens another port of this machine, such as one that a container publishes on `127.0.0.1` on Linux.

The server resolves each name itself and checks every address it gets, so a public name can't lead to a private address. The agent calls other agents through its socket to the server, as before. A blocked connection fails in the agent's tools like a refused one.

`--egress-allow` decides where the agent's own network connections may go. `--private-allow-outgoing` decides which agents on the private network the agent may call through the server.

### Allow a private host

```console
$ a2a-serve config set --egress-allow nas.lan:22
```

The next connection uses the new rule; the agent keeps running. A rule is one of:

- a host, such as `nas.lan`;
- `*.corp.example.com`, for every name under that domain;
- an IP address, such as `192.168.1.20`, or a range, such as `192.168.1.0/24`.

Each can end with a port, such as `:22`. Without a port, the rule allows every port. An IPv6 address takes brackets before a port, such as `[fd00::1]:22`. A pasted URL, such as `http://grafana.lan:3000/d/x`, gives its host and port. Remove a rule with `a2a-serve config unset egress-allow nas.lan:22`.

A host allowed by name may resolve to private addresses, though not to loopback, link-local addresses or this machine. An address rule allows those too. Allow a tailnet host by its name, such as `nas.example.ts.net`. A rule with a tailnet IPv4 address isn't accepted.

| To reach | Run |
| --- | --- |
| A NAS or another LAN host | `a2a-serve config set --egress-allow nas.lan:22` |
| A model server on another machine | `a2a-serve config set --egress-allow 192.168.1.30:11434` |
| A model server on this machine | `a2a-serve config set --egress-allow host.docker.internal:11434`, and point the agent at `host.docker.internal:11434` |

A rule that names `host.docker.internal` reaches that port on this machine's loopback. The ports that containers publish on all addresses stay reachable. With `--egress allowlist`, `host.docker.internal` reaches only the ports of its rules.

### Allow only some hosts

```console
$ a2a-serve config set --egress allowlist \
    --egress-allow generativelanguage.googleapis.com:443 \
    --egress-allow github.com:443 --restart
```

With `--egress allowlist`, the agent reaches only the `--egress-allow` hosts and the hosts of the [inject rules](secrets.md#keep-keys-out-of-the-sandbox) whose secrets the container gets. Include the API host of the agent's model provider in one of them, or the agent can't answer. Start from the hosts the agent needs:

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

`a2a-serve status` lists the hosts it blocked, such as those of the agent's MCP servers. The server doesn't resolve names that no rule lists, and a range allows only connections to its addresses. A name in it needs a rule of its own. `a2a-serve config unset egress --restart` goes back to the internet.

`--egress allowlist` needs the egress boundary. Where it doesn't apply, the agent doesn't start.

### See what was blocked

`a2a-serve status` counts the blocked connections and shows the latest:

```console
$ a2a-serve status
...
  egress   the internet and nas.lan:22; other private networks blocked
           ! 3 connection(s) blocked since the server started, latest 192.168.1.10:22, nas.lan:443 (allowed, retry), nsa.lan:443 (doesn't resolve)
             allow one with a2a-serve config set --egress-allow 192.168.1.10:22
```

A destination stays listed until a connection to it succeeds. One that a rule now allows says `(allowed, retry)`. For a name that resolves to a private address, the suggested rule names the address, or `host.docker.internal` for this machine. An address on the tailnet gets no suggested rule: allow the host by its tailnet name.

`a2a-serve logs` has one line for each blocked connection:

```console
$ a2a-serve logs | grep egress
... level=WARN msg="egress blocked" sandbox=a2a-coder-1a2b3c4d5e destination=192.168.1.10:22 reason=ip-not-on-allowlist
```

Allowed connections are logged too when the server runs with `a2a-serve start --verbose`. A container logs at most 600 decisions a minute. The server logs the number of the others.

| Message | Reason | Meaning |
| --- | --- | --- |
| `egress blocked` | `not-on-allowlist` | With `--egress allowlist`, no rule names the host. For `host.docker.internal`, no rule names the port, and no container publishes it on all addresses ([which ports](#network)). |
| `egress blocked` | `port-not-allowed` | A rule names the host for another port. |
| `egress blocked` | `ip-not-on-allowlist` | The address is private, and no rule allows it. |
| `egress blocked` | `resolved-address-denied` | No address of the name may be reached. `addresses` lists them. |
| `egress blocked` | `resolve-failed` | The name doesn't resolve. |
| `egress blocked` | `busy` | The container has too many open connections. |
| `egress failed` | `dial-failed` | The destination doesn't accept the connection. |

### Limits

- The sandbox has TCP only. UDP is dropped, apart from DNS. QUIC and HTTP/3 clients fall back to TCP, and `ping` doesn't work.
- The sandbox answers DNS queries for A and AAAA records with addresses of its own, and the server resolves the name when the agent connects. Other record types, such as SRV and TXT, get empty answers.
- A container holds at most 256 open connections, and new ones wait beyond 50 per second. A connection that carries no data for an hour is closed.
- `--egress unrestricted` gives the sandbox Docker's own network, with the LAN and the tailnet. Use it for tools that need UDP. It takes effect on restart, as every change of `--egress` does.

### Where it applies

The server routes the sandbox's connections with Docker, rootless Docker and Docker Desktop.

- On Linux, the container gets the socket of its connections and runs the server's binary to set up its network. This needs an a2a-serve built without cgo, as the releases are, an image for the same CPU architecture, and the `tun` kernel module.
- With Docker Desktop, which can't mount the server's sockets, an egress container runs next to each agent's container and forwards its connections to the server. Its name ends in `-egress`, and it starts again when it stops. Its image is `ghcr.io/a2aproject/a2a-serve`, with the version of your a2a-serve. When the pull fails, the server builds the image from the a2a-serve repository at that version, which takes a few minutes the first time. `A2A_SERVE_EGRESS_IMAGE` names an image of your own, such as one built from a work tree with `docker build -f internal/sandboxnet/Dockerfile .`.

Otherwise the sandbox keeps Docker's own network, and `start`, `status` and `doctor` warn about it. With `--egress allowlist`, the agent doesn't start.

At each start, the server also runs a short check: a container with the agent's settings whose launcher sets up the network, drops its capabilities and exits. When that fails, egress control is off until the server stops, and the sandbox keeps Docker's own network. This happens with a runtime that refuses `NET_ADMIN` or `/dev/net/tun`, such as some Podman setups. `start` and the log say what failed, and so do `status` and `doctor`. The next start checks again, so the boundary comes back once the cause is fixed. Each start of an agent's container checks the same way. With `--egress allowlist`, the agent doesn't start, and `--egress unrestricted` skips the check.

On Windows, the server doesn't read the routing table: it counts the subnets of its own interfaces as private, and misses networks that a VPN routes.

`--sandbox local` has no egress rules. The agent reaches every network you reach.
