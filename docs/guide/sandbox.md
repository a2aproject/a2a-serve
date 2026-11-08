# Sandbox

By default the agent runs in Docker:

- The workspace, the directory that holds `.a2a`, is mounted at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config.
- The `.a2a` directories of other agents in the workspace and the mounts also appear empty, so the agent can't read their secrets or reach their control sockets. The server searches the mounts before it starts each container, and the start output lists the agents it found. It refuses a mount with more than 500,000 directories, which would take too long to search.
- `a2a-serve start` refuses to create an agent inside the directory of a running agent. Stop that agent first; its next start hides the new agent. When an agent appears in the mounts of a running agent in another way, such as through `--mount`, the server refuses to start more containers for the running agent until you run `a2a-serve restart`.
- The server refuses a workspace or a mount that is your home directory, `/` or a directory that holds your home directory, because the agent could read your credentials and the secrets of every agent in it. Use a project directory. `--allow-broad-mount <path>` mounts that path anyway, and the agents in it stay visible to the agent.
- The server checks the mount sources, `.a2a` and the agent home directory before it starts each container. It refuses a path that goes through a symlink inside a writable mount, because the agent can repoint that symlink. The error names the resolved path to configure.
- The server also remembers the agent directory, `.a2a`, `state` and each agent home directory when it first uses them. When the agent renames one of them through a writable mount and creates another in its place, the server refuses to start a container until you check the directory and restart the server.
- `state/home` is the agent's home directory. Each authenticated user gets a container with a home of their own, and [guests](#guests) share one more. [Sandbox rules](#what-each-callers-container-gets) set how much of the workspace and which secrets those containers get.
- The container runs as your user, with all capabilities dropped and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Every conversation can read the others, and the agent can read the `.a2a` directories of every agent on the machine, its own included. The server only warns when the workspace holds other agents. Use it only when you trust the agent and everyone who can call it. On Linux, the server kills the processes that an agent process started, such as tool commands, once that agent process exits.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

## What each caller's container gets

Each caller's conversations run in an agent container. You and your owner pairings share yours, guests share one, and every other caller gets one of their own.

<a id="guests"></a>Guests are the callers without a token or account: anonymous visitors on the public URL with `--public-allow-incoming anyone`, and strangers in open chats. [Identities](identities.md#anonymous-callers) calls them anonymous callers.

Without sandbox rules:

- your container and the containers of pairings, members of the private network and `--allow-email` sign-ins get the shared workspace and every `--secret` value;
- the container of guests gets a read-only workspace and no secrets.

Sandbox rules set what chosen callers get, such as a read-only workspace for a pairing, or a key of their own for guests. "Other callers" in the output are the callers who aren't owners or guests and whom no rule matches; they get the default.

`a2a-serve status` shows what each kind of caller gets:

```
  sandbox  you: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           emails matching *@example.com: own copy of the workspace, secrets ANTHROPIC_API_KEY
           other callers: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           guests: read-only workspace, no secrets
```

These lines appear when sandbox rules exist or guests may call; without them, every caller gets the same. `a2a-serve start` shows them when others may call the agent, and says what guests get when no rule covers them. `config set`, `config edit` and the `pair` commands print them as "what their agent containers get:" after "who may call:". `pair get` shows what the pairing's container gets, and `pair invite` shows it when rules change it. When the agent's model needs a key that a caller's container doesn't get, these commands warn that the agent can't reach its model for them and say how to give one.

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

The agent sees `ANTHROPIC_API_KEY` in the container of guests, with the value of `GUEST_ANTHROPIC_KEY`, and no other secrets. Your container and the containers of other callers keep theirs. A new secret loads at restart, so the rule waits for it, and `status` lists it as pending until then.

To share your own key instead, which takes effect at once:

```sh
a2a-serve config set --guest-secrets ANTHROPIC_API_KEY
```

`--guest-secrets` replaces the list each time, and `config unset guest-secrets` resets it. A pairing gets a key of its own the same way, with `a2a-serve pair set <name> --secrets ANTHROPIC_API_KEY=secret:<STORED_KEY>`.

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
- is deleted by `pair rm`, and after 30 days without use for callers without a pairing. The copies of guests and of your container are deleted once their rule no longer gives them `own` storage.

The server keeps at most 100 copies. A caller whose copy would be the 101st gets an error that suggests `read-only` storage.

The agent's instructions name its storage mode and say how to return work in it.

Storage rules need the Docker sandbox. With `--sandbox local`, every caller's agent runs on your machine as you: rules still keep secrets out of its environment, but it can read them in `.a2a/state/secrets`, and `status` says so.

### When a change applies

A running agent applies every change of the rules at once, also those made with `a2a-serve config edit`.

A change that gives a caller less ends the tasks in their container and stops it, so that they can't keep the access they had by keeping a task waiting. Their next message starts the container with the new rule. A secret the agent already saved in its home stays there, so rotate a secret you take away. Another change reaches a container when its current tasks end. Until then, it takes only answers to the tasks that wait in it, and other messages get an error that asks the caller to send them again. Your own container, which the `self` rule covers, applies a change at `a2a-serve restart`, and `status` notes a change waiting for it.

A conversation runs in the container of the user who started it. When someone else sends a message in it and their rule gives less than that container does, the server refuses the message and suggests that they start a conversation of their own.

`${VAR}` in `.mcp.json` takes the `--env` values and the secrets that the conversation's container gets. An MCP server that needs a variable the container doesn't get is left out of that conversation ([MCP and Skills](mcp-and-skills.md)).

[Security](../advanced/security.md#the-sandbox) lists what the sandbox doesn't protect yet.
