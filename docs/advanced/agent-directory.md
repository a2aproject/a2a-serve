# Agent Directory

Each agent keeps its settings and state in `.a2a` in its project directory.

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
        ├── homes/         # a home for the agent process of each authenticated user
        ├── home-anonymous/ # the home of the agent process for anonymous callers
        ├── mcp-mask.json  # the .mcp.json with no servers that the sandbox shows the agent
        ├── server.log     # rotated at 10 MB; the previous log is server.log.1
        ├── server.pid     # PID of the running server, so that stop works while it starts
        └── server.sock    # control socket for a2a-serve commands
```

`a2a-serve` commands talk to the running agent over `server.sock`.

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
        ├── relay.log            # agentmesh-one's output, and the log of a relay service
        ├── relay.lock           # held by the running relay
        └── relay.sock           # control socket for a2a-serve relay commands
```
