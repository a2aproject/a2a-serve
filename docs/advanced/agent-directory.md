# Agent Directory

Each agent keeps its settings and state in `.a2a` in its project directory. This page shows what lives where.

```
~/src/app/
├── …project files…        # the workspace
└── .a2a/
    ├── config.yaml        # agent settings, no secrets; can be committed
    ├── recipe.lock        # the recipe the agent came from, if any; can be committed
    ├── .gitignore         # "state/"
    └── state/             # never committed
        ├── server.db      # tasks, sessions, tokens, contacts, nodes
        ├── secrets        # tokens and API keys
        ├── tsnet/         # Tailscale identity, so the URL survives restarts
        ├── agentmesh/     # Agent Mesh identity and relay sign-in
        ├── home/          # the agent's home directory
        ├── homes/         # a home for the agent process of each member
        ├── policy/        # settings the agent can't override in its containers
        ├── server.log     # the server's log
        ├── server.sock    # control socket for a2a-serve commands
        └── harness/       # a socket per agent process, through which it reaches the server
```

`a2a-serve` commands talk to the running agent over `server.sock`. Other files in `state/`, such as lock files and the SQLite companions of `server.db`, need no attention.

`recipe.lock` records the [recipe](../guide/recipes.md) the agent was created from, with a hash of each of its files. It holds no secrets. The sandbox hides `.a2a`, so the agent can't edit it.

## Relay directory

A [relay](../guide/relays.md) keeps its settings and state in `.a2a` too. A directory holds either an agent or a relay, so relay secrets stay out of agent backups.

```
~/relay/
└── .a2a/
    ├── relay.yaml               # relay settings, no secrets; can be committed
    ├── .gitignore               # "state/"
    └── state/relay/             # never committed
        ├── secrets              # the admin token and the PostgreSQL DSN
        ├── agentmesh/           # agentmesh-one's data: database, router key
        ├── backups/             # database backups taken before upgrades
        ├── relay.log            # agentmesh-one's output
        └── relay.sock           # control socket for a2a-serve relay commands
```
