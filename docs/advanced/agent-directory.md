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
        ├── server.log
        └── server.sock    # control socket for a2a-serve commands
```

`a2a-serve` commands talk to the running agent over `server.sock`.
