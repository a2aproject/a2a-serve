# Sandbox

By default the agent runs in Docker:

- The workspace, the directory that holds `.a2a`, is mounted at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config.
- The `.a2a` directories of other agents in the workspace and the mounts also appear empty, so the agent can't read their secrets or reach their control sockets. The server searches the mounts before it starts each container, and the start output lists the agents it found. It refuses a mount with more than 500,000 directories, which would take too long to search.
- `a2a-serve start` refuses to create an agent inside the directory of a running agent. Stop that agent first; its next start hides the new agent. When an agent appears in the mounts of a running agent in another way, such as through `--mount`, the server refuses to start more containers for the running agent until you run `a2a-serve restart`.
- The server refuses a workspace or a mount that is your home directory, `/` or a directory that holds your home directory, because the agent could read your credentials and the secrets of every agent in it. Use a project directory. `--allow-broad-mount <path>` mounts that path anyway, and the agents in it stay visible to the agent.
- The server checks the mount sources, `.a2a` and the agent home directory before it starts each container. It refuses a path that goes through a symlink inside a writable mount, because the agent can repoint that symlink. The error names the resolved path to configure.
- The server also remembers the agent directory, `.a2a`, `state` and each agent home directory when it first uses them. When the agent renames one of them through a writable mount and creates another in its place, the server refuses to start a container until you check the directory and restart the server.
- `state/home` is the agent's home directory. Each authenticated user gets a container with a home of their own, and [anonymous callers](identities.md#anonymous-callers) share one more.
- The container runs as your user, with all capabilities dropped and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Every conversation can read the others, and the agent can read the `.a2a` directories of every agent on the machine, its own included. The server only warns when the workspace holds other agents. Use it only when you trust the agent and everyone who can call it.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

[Security](../advanced/security.md#the-sandbox) lists what the sandbox doesn't protect yet.
