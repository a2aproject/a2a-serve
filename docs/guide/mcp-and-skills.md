# MCP and Skills

The agent gets the MCP servers and skills of its project.

- **MCP servers** come from `.mcp.json` in the workspace, read when a conversation starts:

  ```json
  {"mcpServers": {
    "github": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
               "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "${GITHUB_TOKEN}"}},
    "docs":   {"type": "http", "url": "https://docs.example.com/mcp",
               "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
  }}
  ```

  `${VAR}` and `${VAR:-default}` take their values from [`--secret`](secrets.md) and `--env`, so tokens stay out of the file: `a2a-serve config set --secret GITHUB_TOKEN`. A server with a variable that has no value is skipped, and `status` names the variable. Servers that run a command run inside the sandbox.

  To turn a server off, run `a2a-serve config set --disable-mcp github`. To turn it back on, run `a2a-serve config unset disable-mcp github`. The change applies to the next conversation, and `status` shows the server as off. The first-run wizard turns off the servers whose variables you skip.

  The server passes these servers to the agent when a conversation starts. The Docker sandbox shows the agent a `.mcp.json` with no servers, so the agent can't start servers from the file on its own. On the first start, a workspace without `.mcp.json` gets one with no servers.
- **Skills** in `.claude/skills`, `.opencode/skills` and `.agents/skills` work with both opencode and Claude Code. When two skills share a name, the one in `.claude/skills` wins. With `--sandbox local`, only the folders your agent reads on its own work.

The server reads both inside the workspace only. It refuses a `.mcp.json` that is a symlink to a file outside the workspace, and leaves out skills behind such symlinks, because the agent can create them.

`a2a-serve status` lists the MCP servers and skills the agent found.

Not in this version: MCP servers and skills from your home directory, and limiting a server to some roles.
