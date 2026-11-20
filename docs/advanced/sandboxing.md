# Sandboxing

This page summarizes how a2a-serve sandboxes the agent and controls its traffic.

## General

- Workspace is the directory where a server was started. In the sandbox it's mounted under the same path.
- Server keeps its state in the `./.a2a` directory. Full layout is explained in [Agent Directory](agent-directory.md). The directory appears empty from the sandbox.
- A launcher is the server binary copied to `.a2a/state/…/launcher/a2a-init`. This copy is mounted to the sandbox to configure it on startup.
- A server can run multiple containers: one for each identity and a shared one for guests if they are allowed.
- Policies matching identities are applied to secrets and storage mode (shared, read-only, own copy).
- Egress control implements the [agents.net](https://github.com/aojea/agents.net) specification.

## Container Start

### Linux

To start a container the server:

1. Pulls an agent image from the docker registry. The image has a2a-cli and commonly needed utilities in it: [example](../../internal/agentprofile/images/opencode/Dockerfile).
1. Prepares the agent home (`./.a2a/state/homes/<user>`). That's where sandbox `$HOME` will point and where sessions will live.
1. Opens two unix sockets for the container: the a2a-cli socket and the all-egress socket.
1. Writes the env vars and secrets into a private docker env file with placeholders for injected keys, and deletes the file when the container stops.
1. Runs `docker run` with `--network none`, `--device /dev/net/tun` and net-admin capabilities.
1. The `docker run` command bind-mounts the workspace, the agent home, and both socket directories as read-only. An empty read-only tmpfs is mounted on top of `.a2a` to mask it in the sandbox.
1. The `docker run` command replaces the entrypoint with `/run/a2a-init` (i.e. the launcher).

The launcher:

1. Receives the egress unix socket path, agent uid and gid, and the original entrypoint (eg. `opencode acp`).
1. Adds the egress CA to the container's trust stores (for openssl, bun, npm, git etc.).
1. Creates `tun0` network interface and routes all IPv4 and IPv6 traffic through it.
1. Points `/etc/resolv.conf` at its own virtual DNS server.
1. Starts a user-space TCP stack that reads the TUN device.
1. Drops every capability, switches to agent uid and gid.
1. Starts the original entrypoint as an unprivileged child.
1. Stdio of `docker run` is now owned by agent's acp process and managed by [a2aacp.Executor](https://github.com/a2aproject/a2a-go/blob/main/integrations/executor/a2aacp/executor.go#L159).

### macOS

Docker Desktop runs containers in a small Linux virtual machine, while the server runs on macOS outside it. That creates two problems:

1. A Unix socket on the Mac can't be mounted into a container in the VM, so the agent can't reach the server's sockets directly.
1. The Mac's a2a-serve is a macOS binary, which can't run inside a Linux container as the launcher.

The fix is a small helper container per agent container, called the `*-egress` container. It runs inside the VM next to the agent container and carries the agent's socket traffic out to the server on the Mac. Its image contains the Linux build of a2a-serve, which serves as the launcher.

The boundary that decides each connection still runs inside the server on the Mac.

The server:

1. Listens on two random TCP ports on the Mac's localhost.
1. Pulls linux `ghcr.io/a2aproject/a2a-serve` with the same version as its own, or the image of its commit for builds that aren't releases.
1. Starts `<name>-egress` from that image, no capabilities, but with Docker's normal network.
1. The egress container creates volumes for unix sockets (`egress.sock`, `a2a.sock`) and one that holds the Linux a2a-serve binary.
1. The container can reach the Mac's loopback ports through `host.docker.internal`.
1. The server gives the agent container a random key for its a2a-cli socket, and the egress container another one for its egress socket. On Linux, the socket proves which container sent a message, but on Mac every process can call on the loopback port.

## Live Workspaces

A live workspace is a file tree that another program holds, which the agent's container shows at the workspace path. Every file operation of the agent goes to the program, so it sees the agent's changes as they happen, and the agent sees its changes. The program serves the tree with the [workspace protocol](../reference/workspace-protocol.md), or passes it in Go to `sandbox.Served` or `a2aserve.Live`.

The launcher mounts the tree with FUSE:

1. The server serves the tree on a second Unix socket, the fs socket, next to the egress socket. A read-only workspace refuses changes on the socket too.
1. `docker run` adds `--device /dev/fuse` and the `SYS_ADMIN` capability.
1. The launcher mounts FUSE at the workspace path before it drops its capabilities. It keeps serving the mount after it drops them, and sends each operation over the fs socket.
1. The agent inherits no capability and no file of the launcher, so it can't answer the mount's calls or unmount it.
1. Docker relaxes its seccomp profile for a container with `SYS_ADMIN`, so the launcher installs its own filter before it starts the agent. The filter refuses new namespaces, mounts, keyrings and bpf, as Docker's default profile does, and ptrace and the other calls that read or change another process.

The agent can reach the fs socket directly. The server applies the masks and the read-only setting there too, follows no symlink, and refuses symlinks that lead into masked paths.

Files are written whole. The writes to an open file go to a buffer in the container, and reach the program when the agent closes, flushes or syncs the file. The last write wins. Many network file systems use these close-to-open semantics, and they suit editors and build tools. A program that changes a file in place through `mmap`, such as a database, changes it in the tree when it closes the file. The buffers are bounded per file and per container, as [Limits](../reference/limits.md#live-workspace) lists.

With `a2aserve.Live`, the masks of the workspace apply to the tree before the agent sees it. `.mcp.json` and the agent's configuration files appear empty and read-only, the agent's configuration folders, such as `.opencode`, start empty and keep the agent's writes in the container, and `.a2a` directories are hidden at any depth.

A live workspace needs the launcher, so it runs only behind the egress boundary. With `--egress unrestricted`, or where the boundary can't work, the container doesn't start, and the error says so. Syncing the workspace at turn boundaries works on every runtime.

| Runtime | Live workspace |
| --- | --- |
| Docker rootless on Linux | Yes, verified |
| Docker rootful on Linux | Yes, verified, with the AppArmor profile `a2a-serve-live` where Docker applies AppArmor. The profile is Docker's default with the FUSE mount allowed. `a2a-serve-sidecar` uses it once it is [loaded](../reference/a2a-serve-sidecar.md#workspace), and embedders name it with `DockerOptions.ServedAppArmor`. |
| Docker Desktop | No: the egress container doesn't carry the fs socket yet |
| bubblewrap on Linux | Yes, verified. It needs `/dev/fuse` and unprivileged user namespaces. |
| gVisor (`runsc`) | No: gVisor's FUSE can't map files into memory, so git and the programs in the workspace fail. The container doesn't start, and the error says so. Verified. |
| Platform-provided sandbox | Where the platform mounts a `Served` source, as [Platform-provided Sandboxes](#platform-provided-sandboxes) describes. Not verified yet. |
| Local | No: the agent runs on the host, with no mount namespace |

## Bubblewrap

`a2a-serve-sidecar --sandbox bwrap`, and the `sandbox.Bubblewrap` runtime in Go, run the agent of this machine with bubblewrap, in new user, PID, IPC, UTS, cgroup, mount and network namespaces. No daemon and no image are involved, and no privileges beyond unprivileged user namespaces.

The agent sees:

- `/usr` and the other system directories, read-only.
- Of `/etc`, only the users and groups, name resolution, the dynamic linker's files, the alternatives, the CA certificates and the time zone. The host's credentials and keys in `/etc` stay out.
- Private `/tmp`, `/var/tmp` and `/run`, `/proc`, and a minimal `/dev`.
- The workspace and mounts, its home at `/home/agent`, and the directories of its command and the `a2a` CLI, read-only. Hidden paths appear empty, as in a container.

With the egress boundary, the launcher starts first, as root of the sandbox's user namespace, which is the user who started the sandbox. It gets `/dev/net/tun`, the `NET_ADMIN` and `SETPCAP` capabilities, and, with a live workspace, `/dev/fuse` and `SYS_ADMIN`. It runs as PID 1 of the sandbox, sets up the network as in a container, mounts the live workspace, drops every capability, and installs the seccomp filter of live workspaces, which refuses new namespaces, mounts and ptrace. Then it starts the agent, so every process of the agent has the filter. The network namespace has only loopback and the TUN device, so every connection goes through the boundary. Where the launcher can't set this up, the sandbox doesn't start. It never falls back to the host's network.

Ubuntu 23.10 and later restrict unprivileged user namespaces with AppArmor (`kernel.apparmor_restrict_unprivileged_userns=1`). Where bwrap's AppArmor profile lets it make the namespaces, the profile can still deny the capabilities in them, so the launcher can't create the TUN device or mount FUSE. To run the sandbox there, give bwrap an AppArmor profile that allows user namespaces with their capabilities, or set `kernel.apparmor_restrict_unprivileged_userns=0`, which lifts the restriction for every program. The errors of `a2a-serve-sidecar` name this setting when it applies.

Without an egress endpoint, or with an unrestricted policy, the sandbox has only loopback and doesn't start the launcher. In Go, `BubblewrapOptions.HostNetwork` shares the host's network in that case. The agent then reaches every service of the host on loopback and on abstract Unix sockets, such as the X11 display, which can read and send keystrokes. Bubblewrap applies no memory, CPU or process limits.

## gVisor

`a2a-serve-sidecar --sandbox gvisor`, and `DockerOptions.Runtime` set to `runsc` in Go, run the Docker containers with [gVisor](https://gvisor.dev)'s runtime `runsc`. gVisor runs the container on a kernel of its own in user space, so the agent's system calls don't reach the host's kernel.

The container starts as other Docker containers do. The launcher sets up the TUN device in gVisor's network stack, drops every capability, and starts the agent, so every connection goes through the egress boundary. gVisor refuses connections to the host's Unix sockets by default, so Docker must run `runsc` with `--host-uds=open` for the container to reach its egress socket:

```console
$ sudo runsc install -- --host-uds=open
$ sudo systemctl restart docker
```

Without it, the sandbox doesn't start, and the error names the flag. Live workspaces don't work under gVisor, and Docker Desktop's egress container doesn't work with another runtime yet. Verified with rootful Docker on Linux.

## Platform-provided Sandboxes

A platform that already isolates the agent, such as a sandbox service or a microVM runtime, embeds the core in Go and implements `sandbox.Runtime`. The core passes the workspace and the other mounts in `Spec.Mounts`:

- A `HostDir` source for a directory workspace, and for turn sync, which gives each A2A context a copy in a directory.
- A `Tmpfs` source for the agent's scratch directories.
- A `Served` source for a live workspace.

The platform shows each source at its path. Directory and turn-synced workspaces work wherever the platform can show a directory of the host. Live workspaces work where it can mount a served tree, and its `Create` returns a `sandbox.UnsupportedMountError` elsewhere. The platform enforces egress itself. A runtime that can't route the connections through the endpoint in `Spec.Egress` fails `Create` with `sandbox.ErrNoBoundary`, so give the core no egress boundary there. This isn't verified with a platform yet.

## Calling the Network

When an agent runs a tool which initiates a network request:

1. The launcher's DNS answers with a synthetic address.
1. The tool opens a TCP connection to the synthetic address, and the launcher's TCP stack sitting behind `tun0` accepts the connection.
1. It opens a new connection on the egress socket and sends `HTTP CONNECT <name>:<port>`.
1. The boundary checks the destination against configured policies, resolves the name, dials the address and starts copying bytes in both directions.
1. The tool's TLS handshake runs end to end through the tunnel.

For requests with injected credentials the boundary:

1. Terminates the tool's TLS with a certificate for that host. This is done by presenting a leaf certificate of the target domain signed by the injected trusted CA created on container start.
1. For a model key, the boundary deletes `Authorization`, `X-Api-Key`, `X-Goog-Api-Key`, `Api-Key` and the provider's key query parameter, then sets the real key if the path is one allowlist of inference endpoints.
1. For a header rule, the boundary replaces that one header with the value holding the secret.
1. Opens its own TLS connection to the real host, verified against the host machine's system roots, and dials only the addresses it checked, then sends the request over its own verified TLS connection.

## Calling Peer Agents

The agent runs a2a to the agent's "virtual address" injected into the conversation context and the a2a CLI sends the request through a dedicated unix socket.

Sockets are tagged with the container's identity.

The server:

1. Checks that the turn's user may call agents and that the target is an existing contact.
1. Calls the target agent with its own access token, so the token never enters the container.
1. Relays the callee's permission questions to the human who initiated the turn.

## Incoming Requests

A message arrives on one of the server listeners: localhost, private or public network.

The server authenticates the caller and passes the message to an a2a-go server which runs in a2a-serve.

Auth gives the caller a role: owner, member or guest which is mapped to a container where the agent runs. Running containers are managed by [a2aacp.Executor](https://github.com/a2aproject/a2a-go/blob/main/integrations/executor/a2aacp/executor.go#L159).

The agent works on the message and writes its replies, tool calls and permission questions to stdout, which are translated to A2A data model and streamed back to the caller.

## Caller Identities

Every request gets an identity and a policy turns it into a role: owner, member or guest. Calls through another agent get at most the member role.

### Pairing

A pairing works on any network listener, including the public URL, and names a person you chose without needing an account anywhere.

1. You run `a2a-serve pair invite`, which creates a pairing in `server.db` with a role.
1. The server mints a single-use code like `K7Q2-9XF4`, valid for 15 minutes, and stores it.
1. The invited party runs `a2a-serve pair join <agent-url> <code>`, which sends the code to the agent's public `/pair` route.
1. The server consumes the code and issues a caller token storing its hash.
1. The invited party saves the token in `~/.config/a2a-serve/credentials.json`, and the a2a CLI sends it as `Authorization: Bearer`.
1. On each request the token's hash is used to find a pairing.

Webhook nodes (eg. grok) are also identified through a pairing. Messenger platforms can't attach a Bearer header, so the platform's sender account ID, joined once with a code, stands in for the token.

A messenger adapter reports the sender's account ID verified by the platform and it's used to look up an implicit pairing.

Pairings can be deleted, tokens can be rotated.

### Local Identity

At each start, the server issues a token of the built-in local pairing and saves it in `credentials.json`.

A request with that token counts as you only when it arrives on the local URL.

The server deletes the saved token when it stops.

Without the token we couldn't tell a local call from a Tailscale Funnel connection or a relay tunnel.

### Network Identity

Network abstraction implementations need to provide WhoIs mechanisms for identifying nodes on the private network. By default, devices on the private network are considered trusted. `--private-allow-incoming paired` hardens this rule such that private network callers all require an explicit pairing.

### Google Sign-in

Can be enabled with `--allow-email anna@example.com` or `*@example.com`.

The agent card on the public URL advertises a Google sign-in. With the shared Google app, the client signs in to Google and exchanges the ID token at the agent. With `--google-client-id`, the agent runs the OAuth flow itself. Either way the server issues its own token bound to the email, stored hashed, and the client uses it as a Bearer token.
