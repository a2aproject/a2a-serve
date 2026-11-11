# MCP and Skills

The agent gets the MCP servers and skills of its project.

## MCP servers

MCP servers come from `.mcp.json` in the workspace, read when a conversation starts:

```json
{"mcpServers": {
  "github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
             "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"}},
  "docs":   {"type": "http", "url": "https://docs.example.com/mcp",
             "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
}}
```

`${VAR}` and `${VAR:-default}` take their values from [`--secret`](secrets.md) and `--env`, so tokens stay out of the file:

```sh
a2a-serve config set --secret GITHUB_TOKEN
```

A server with a variable that has no value is skipped, and `a2a-serve status` names the variable. A server that needs a secret that the caller's container doesn't get is left out of that caller's conversations ([sandbox rules](sandbox.md#what-each-callers-container-gets)). Servers that run a command run inside the sandbox.

To turn a server off or on:

```sh
a2a-serve config set --disable-mcp github
a2a-serve config unset disable-mcp github
```

The change applies to the next conversation, and `status` shows the server as off.

## Skills

Skills in `.claude/skills`, `.opencode/skills` and `.agents/skills` work with both opencode and Claude Code. With `--sandbox local`, only the folders your agent reads on its own work.

`a2a-serve status` lists the MCP servers and skills the agent found.

Not in this version: MCP servers and skills from your home directory, and limiting a server to some roles.
