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
- The agent runs as your user, with no capabilities and limits of 4 GB of memory, 2 CPUs and 512 processes (`--memory`, `--cpus`, `--pids`). With rootless Docker it runs as container root, which is your user on the host.
- The agent has no network of its own. Its connections go through the server, which decides each one by its destination ([Network](#network)).

The image is pulled from `ghcr.io/a2aproject`. If the pull fails, a2a-serve builds it from a Dockerfile in its binary. `--image` uses another image, which needs a compatible `a2a` CLI.

`--sandbox local` runs the agent on the host, as you, with no isolation. Every conversation can read the others, and the agent can read the `.a2a` directories of every agent on the machine, its own included. The server only warns when the workspace holds other agents. Use it only when you trust the agent and everyone who can call it. On Linux, the server kills the processes that an agent process started, such as tool commands, once that agent process exits.

When the agent calls another agent, `a2a` waits up to 5 minutes for the reply. `--env A2ACLI_TIMEOUT=<duration>` changes this.

## Network

On Linux, with Docker or rootless Docker, every connection the agent opens goes through the server. With Docker Desktop, the sandbox keeps Docker's own network ([where it applies](#where-it-applies)). By default:

- The agent reaches the public internet.
- Private destinations are blocked: the tailnet, the LAN, link-local and unique local addresses, loopback, the addresses of this machine, and every network this machine reaches through a route other than its default route, such as a VPN or a bridge.
- `host.docker.internal` reaches the ports that other containers publish on this machine. `--egress-allow host.docker.internal:11434` opens another port of this machine.

The server resolves each name itself and checks every address it gets, so a public name can't lead to a private address. The agent calls other agents through its socket to the server, as before. A blocked connection fails in the agent's tools like a refused one.

### Allow a private host

```console
$ a2a-serve config set --egress-allow nas.lan:22
```

The next connection uses the new rule; the agent keeps running. A rule is one of:

- a host, such as `nas.lan`;
- `*.corp.example.com`, for every name under that domain;
- an IP address, such as `192.168.1.20`, or a range, such as `192.168.1.0/24`.

Each can end with a port, such as `:22`. Without a port, the rule allows every port. An IPv6 address takes brackets before a port, such as `[fd00::1]:22`. A pasted URL, such as `http://grafana.lan:3000/d/x`, gives its host and port. Remove a rule with `a2a-serve config unset egress-allow nas.lan:22`.

A host allowed by name may resolve to private addresses, though not to loopback, link-local addresses or this machine. An address rule allows those too. Allow a tailnet host by its name, such as `nas.example.ts.net`: the sandbox's resolver uses the tailnet's IPv4 range for its own addresses.

| To reach | Run |
| --- | --- |
| A NAS or another LAN host | `a2a-serve config set --egress-allow nas.lan:22` |
| A model server on another machine | `a2a-serve config set --egress-allow 192.168.1.30:11434` |
| A model server on this machine | `a2a-serve config set --egress-allow host.docker.internal:11434`, and point the agent at `host.docker.internal:11434` |

A rule that names `host.docker.internal` reaches that port on this machine's loopback, and limits `host.docker.internal` to the ports of the rules.

### Allow only some hosts

```console
$ a2a-serve config set --egress allowlist \
    --egress-allow generativelanguage.googleapis.com:443 \
    --egress-allow github.com:443 --restart
```

With `--egress allowlist`, the agent reaches only the `--egress-allow` hosts. Include the API host of the agent's model provider, or the agent can't answer. The server doesn't resolve names that no rule lists, and a range allows only connections to its addresses. A name in it needs a rule of its own. `a2a-serve config unset egress --restart` goes back to the internet.

`--egress allowlist` needs the egress boundary. Where it doesn't apply, the agent doesn't start.

### See what was blocked

`a2a-serve status` counts the blocked connections and shows the latest:

```console
$ a2a-serve status
...
  egress   the internet and nas.lan:22; other private networks blocked
           ! 2 connection(s) blocked since the server started, latest 192.168.1.10:22, files.lan:443
             allow the latest with a2a-serve config set --egress-allow 192.168.1.10:22
```

A destination stays listed until a connection to it succeeds. For a name that resolves to a private address, the suggested rule names the address, or `host.docker.internal` for this machine.

`a2a-serve logs` has one line for each blocked connection:

```console
$ a2a-serve logs | grep egress
... level=WARN msg="egress blocked" sandbox=a2a-coder-1a2b3c4d5e destination=192.168.1.10:22 reason=ip-not-on-allowlist
```

Allowed connections are logged too when the server runs with `a2a-serve start --verbose`. A container logs at most 600 decisions a minute. The server logs the number of the others.

| Message | Reason | Meaning |
| --- | --- | --- |
| `egress blocked` | `not-on-allowlist` | With `--egress allowlist`, no rule names the host, or no container publishes the port of `host.docker.internal`. |
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

The server routes the sandbox's connections on Linux, with Docker or rootless Docker, and an a2a-serve built without cgo, as the releases are. Otherwise the sandbox keeps Docker's own network, and `start`, `status` and `doctor` warn about it. This happens:

- with Docker Desktop, where the sandbox can't connect to the server;
- with an a2a-serve built with cgo;
- with an image for a CPU architecture other than a2a-serve's.

`--sandbox local` has no egress rules. The agent reaches every network you reach.
