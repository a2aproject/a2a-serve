# Sandbox

By default the agent runs in Docker:

- The workspace, the directory that holds `.a2a`, is mounted at the same path. `--workspace` picks another directory, and `--mount src[:dst][:ro]` adds more.
- `.a2a` appears empty, so the agent can't read its own secrets, tokens or config.
- The server resolves the mount sources and `.a2a` once, when it starts. When one of them lies inside another writable mount, the agent could replace a directory on its path with a symlink. The server then refuses to start a container until you check the path and restart the server.
- `state/home` is the agent's home directory. Each authenticated user gets a container with a home of their own, and [anonymous callers](identities.md#anonymous-callers) share one more.
- The container runs as your user, with all capabilities dropped and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Every conversation can read the others. Use it only when you trust the agent and everyone who can call it.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

[Security](../advanced/security.md#the-sandbox) lists what the sandbox doesn't protect yet.
