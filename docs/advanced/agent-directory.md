# Agent Directory

Each agent keeps its settings and state in `.a2a` in its project directory.

```
~/src/app/
├── …project files…        # the workspace
└── .a2a/
    ├── config.yaml        # agent settings, no secrets; can be committed
    ├── recipe.lock        # the recipe the agent came from, if any; can be committed
    ├── .gitignore         # "state/"
    └── state/             # never committed
        ├── server.db      # tasks, sessions, tokens, contacts, nodes
        ├── secrets        # tokens and API keys, mode 0600
        ├── tsnet/         # Tailscale identity, so the URL survives restarts
        ├── agentmesh/     # Agent Mesh identity, and the relay sign-in's refresh token
        ├── ads.json       # pairings that agents on the private network advertise
        ├── home/          # the agent's home directory
        ├── homes/         # a home for the agent process of each member
        ├── home-anonymous/ # the home of the agent process for guests, named as in older versions
        ├── mcp-mask.json  # the .mcp.json with no servers that the sandbox shows the agent
        ├── policy/        # the settings that the agent can't override in its containers
        ├── server.log     # rotated at 10 MB; the previous log is server.log.1
        ├── server.pid     # PID of the running server, so that stop works while it starts
        ├── server.lock    # held by the running server
        ├── server.sock    # control socket for a2a-serve commands
        └── harness/       # a socket per agent process, through which it reaches the server
```

`a2a-serve` commands talk to the running agent over `server.sock`.

A Unix socket path can't be longer than about 100 bytes. When the agent directory is deep enough that `server.sock` or a socket in `harness/` would pass that limit, the socket moves to `$XDG_RUNTIME_DIR`, or to `/tmp/a2a-<uid>` when it isn't set. The file name there is derived from the agent directory.

The files ending in `.lock` coordinate the server and `a2a-serve` commands that write the same files. `server.db-wal` and `server.db-shm` belong to the SQLite database. A backup in progress stages its files in a `backup-*` directory. These files need no attention.

`recipe.lock` records the [recipe](../guide/recipes.md) the agent was created from: the repository, ref, path and commit, or the local directory, with a hash of each of the recipe's files and the recipe's `config.yaml`. It holds no secrets. The sandbox hides `.a2a`, so the agent can't edit it.

## Relay directory

A [relay](../guide/relays.md) keeps its settings and state in `.a2a` too. A directory holds either an agent or a relay, so relay secrets stay out of agent backups.

```
~/relay/
└── .a2a/
    ├── relay.yaml               # relay settings, no secrets; can be committed
    ├── .gitignore               # "state/"
    └── state/relay/             # never committed, mode 0700
        ├── secrets              # the admin token and the PostgreSQL DSN, mode 0600
        ├── agentmesh/           # agentmesh-one's data: SQLite database, router key
        ├── agentmesh-version    # the Agent Mesh version that last ran on the data
        ├── backups/             # SQLite backups taken before upgrades; the last 3 are kept
        ├── removed-agents.json  # names of removed invited agents, which a new invite lets back
        ├── relay.log            # agentmesh-one's output, and the log of a relay service
        ├── relay.lock           # held by the running relay
        └── relay.sock           # control socket for a2a-serve relay commands
```
