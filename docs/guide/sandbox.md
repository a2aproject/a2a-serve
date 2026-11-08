# Sandbox

By default the agent runs in Docker:

- The workspace, the directory that holds `.a2a`, is mounted at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config.
- The `.a2a` directories of other agents in the workspace and the mounts also appear empty, so the agent can't read their secrets or reach their control sockets. The server searches the mounts before it starts each container, and the start output lists the agents it found. It refuses a mount with more than 500,000 directories, which would take too long to search.
- `a2a-serve start` refuses to create an agent inside the directory of a running agent. Stop that agent first; its next start hides the new agent. When an agent appears in the mounts of a running agent in another way, such as through `--mount`, the server refuses to start more containers for the running agent until you run `a2a-serve restart`.
- The server refuses a workspace or a mount that is your home directory, `/` or a directory that holds your home directory, because the agent could read your credentials and the secrets of every agent in it. Use a project directory. `--allow-broad-mount <path>` mounts that path anyway, and the agents in it stay visible to the agent.
- The server checks the mount sources, `.a2a` and the agent home directory before it starts each container. It refuses a path that goes through a symlink inside a writable mount, because the agent can repoint that symlink. The error names the resolved path to configure.
- The server also remembers the agent directory, `.a2a`, `state` and each agent home directory when it first uses them. When the agent renames one of them through a writable mount and creates another in its place, the server refuses to start a container until you check the directory and restart the server.
- `state/home` is the agent's home directory. Each authenticated user gets a container with a home of their own, and [anonymous callers](identities.md#anonymous-callers) share one more. [Sandbox rules](#what-each-callers-container-gets) set how much of the workspace and which secrets those containers get.
- The container runs as your user, with all capabilities dropped and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Every conversation can read the others, and the agent can read the `.a2a` directories of every agent on the machine, its own included. The server only warns when the workspace holds other agents. Use it only when you trust the agent and everyone who can call it. On Linux, the server kills the processes that an agent process started, such as tool commands, once that agent process exits.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

## What each caller's container gets

Your agent container, which you and your owner pairings share, gets the workspace and every secret. Every other caller gets a container of their own, and [anonymous callers](identities.md#anonymous-callers) share one. By default those containers get a read-only workspace and no secrets, so a caller can ask the agent questions about the project without changing it and without using your keys.

`a2a-serve status` shows what each kind of caller gets:

```
  sandbox  you: shared workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY
           emails matching *@example.com: own copy of the workspace, secrets ANTHROPIC_API_KEY, GITHUB_TOKEN
           everyone else: read-only workspace, secrets ANTHROPIC_API_KEY (from GUEST_ANTHROPIC_KEY)
```

`a2a-serve start` shows the same lines when others may call the agent. `config set`, `config edit` and the `pair` commands print them as "what their agent containers get:" after "who may call:", and `pair invite` and `pair get` show what the pairing's container gets. Without the Docker sandbox, the lines say "no sandbox", since only the secrets apply.

### Share the agent read-only

Invite someone, with no extra flags:

```sh
a2a-serve pair invite anna
```

Anna's agent container reads the workspace and can't change it. It has no secrets, so the agent needs its own way to reach a model, such as a [separate key](#give-guests-a-separate-model-key). `a2a-serve pair get anna` shows a `sandbox` line, such as `read-only workspace, no secrets`.

### Give a pairing a copy of the workspace and some secrets

```sh
a2a-serve pair invite ben --storage own --secrets ANTHROPIC_API_KEY,GITHUB_TOKEN
a2a-serve pair set ben --storage shared --secrets all
a2a-serve pair set ben --secrets none
```

`pair invite` and `pair set` add or replace the sandbox rule of the pairing. A flag you leave out keeps that part of the rule. `pair set` changes an existing pairing, and `pair rm ben` removes its rule. The names in `--secrets` come from the agent's [`--secret`](secrets.md) list. Owner pairings refuse `--storage` and `--secrets`, because owners share your container. The [`self` rule](../reference/configuration.md#sandbox-rules) in `config.yaml` sets what that container gets.

### Give guests a separate model key

Callers who aren't owners can read a key they receive, so give them a key of their own that you can revoke. Store it as a secret, and pass it to the agent under the name it expects:

```sh
a2a-serve config set --secret GUEST_ANTHROPIC_KEY
a2a-serve config set --caller-storage read-only --caller-secrets ANTHROPIC_API_KEY=secret:GUEST_ANTHROPIC_KEY
```

The agent sees `ANTHROPIC_API_KEY` in those containers, with the value of `GUEST_ANTHROPIC_KEY`. Your own container keeps its key. `config unset caller-storage` and `config unset caller-secrets` reset the two settings.

### Restore the old behavior

Before sandbox rules, every container got the shared workspace and every secret. To give that back to callers who aren't owners:

```sh
a2a-serve config set --caller-storage shared --caller-secrets all
```

`status`, `start` and the commands that change who may call print this command in a note while others may call the agent and no rule of `anyone` exists. `a2a-serve config set --caller-storage read-only` keeps the new default and hides the note.

### Rules for several callers

`--caller-storage` and `--caller-secrets` set the rule for `anyone`, and `pair` sets the rule of one pairing. Write the rest, such as a rule for a whole email domain, in `a2a-serve config edit`, in the `policy.sandbox` list that the [configuration reference](../reference/configuration.md#sandbox-rules) describes. `a2a-serve config unset sandbox-rules` removes every rule except the one for `anyone`, including the rules of pairings and of `self`.

### Storage modes

| Mode | Workspace | Mounts |
| --- | --- | --- |
| `shared` | The workspace, as you mounted it. | As configured, writable unless `:ro`. |
| `read-only` (default) | The workspace, read-only. | Read-only. |
| `own` | A private copy of the workspace. | Read-only. |
| `empty` | An empty private directory. | None. |

In every mode, the agent home directory and `/tmp` stay writable.

An `own` copy is made once, when the caller's container first starts. Nothing is copied back to your workspace, so the caller gets work back in the agent's replies. The copy:

- leaves out every `.a2a` directory;
- holds at most 1 GiB in at most 200,000 entries. When the workspace is larger, the container fails to start with an error that suggests `read-only`;
- lives in `.a2a/state/workspaces/`, and [backups](backups.md) leave it out;
- is deleted by `pair rm`, and after 30 days without use for callers without a pairing. The copy of anonymous callers is deleted once their rule no longer gives them `own` storage.

The server keeps at most 100 copies. A caller whose copy would be the 101st gets an error that suggests `read-only` storage.

The agent's instructions name its storage mode and say how to return work in it.

Storage needs the Docker sandbox. With `--sandbox local`, only the secrets apply, and `status` prints a note.

### When a change applies

A running agent applies every change of the rules at once, also those made with `a2a-serve config edit`.

A change reaches the container of a caller when its current tasks end. Idle containers stop within seconds and start with the new rule on the next message. Until a busy container restarts, it takes only answers to the tasks that wait in it, and other messages get an error that asks the caller to send them again. Your own container, which the `self` rule covers, applies a change at `a2a-serve restart`, and `status` notes a change waiting for it.

A conversation runs in the container of the user who started it. When someone else sends a message in it and their rule gives less than that container does, the server refuses the message and suggests that they start a conversation of their own. For example, an owner whose `self` rule gives fewer secrets than a user's container gets this.

`${VAR}` in `.mcp.json` takes the `--env` values and the secrets that the conversation's container gets. An MCP server that needs a variable the container doesn't get is left out of that conversation ([MCP and Skills](mcp-and-skills.md)).

[Security](../advanced/security.md#the-sandbox) lists what the sandbox doesn't protect yet, including what `read-only` and `own` callers can still read.
