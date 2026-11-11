# Recipes

A recipe is a directory with a ready-made agent, and `a2a-serve recipe` creates an agent from one with a single command.

## Create an agent from a recipe

```sh
a2a-serve recipe https://github.com/acme/agents/tree/v1.2/experts/elk --name elk
cd elk
a2a-serve start
```

The source can be a tree URL in the GitHub layout, a repository URL, a git URL such as `git@github.com:acme/agents.git#v1.2:experts/elk`, or a local directory. The version of a recipe is a git ref. The command fetches the recipe with your `git`, so your SSH keys and credential helpers apply.

The command then:

1. Shows what the agent will run: the agent, the sandbox, the mounts, the MCP servers, the skills and the approvals. It marks risky entries with `!`.
2. Asks before it creates the agent in `./<name>`, or in `--dir`. `--yes` skips the question, and `--dry-run` only prints the review.
3. Asks for the secrets the agent lacks, and whether to use each optional MCP server. A value set in your shell is offered first.
4. Saves the agent.

Without a terminal, pass `--yes` and give the values with `--secret`, `--env` or the environment. To create an agent without an MCP server, add `--disable-mcp <server>`.

The flags of `a2a-serve config set` apply on top of the recipe, such as `--memory 8g`. The [command reference](../reference/commands/recipe.md) lists every flag, and the [observability agent example](../../examples/04-observability-agent/README.md) walks through a whole setup.

A recipe sets what the agent is. You decide where it is reachable and who may call it, so add messengers and networks afterwards, as for any agent:

```sh
a2a-serve messenger add telegram --token env:TELEGRAM_BOT_TOKEN
a2a-serve config set --private tailscale
```

A recipe may let the agent's MCP tools run without asking, for every caller, and the review shows it. The agent acts with the credentials you give it, so give it credentials limited to the data it needs.

## Update an agent

Run `a2a-serve recipe` without a source in the agent's directory to update the agent to a newer version of its recipe:

```sh
cd elk
a2a-serve recipe
```

A branch moves to its newest commit. A tag or a commit stays where it is. To move to a new version, pass the source with the new ref:

```sh
a2a-serve recipe https://github.com/acme/agents/tree/v1.3/experts/elk
```

The update shows what changes and asks before it saves. It keeps your changes:

- A file you didn't change takes the recipe's new version.
- A file changed both here and in the recipe stays as it is here. The recipe's version is saved next to it as `<file>.recipe`. Compare the two, edit your file, and delete the copy.
- Settings merge the same way, and your value wins when both sides changed one.
- The command asks for the secrets of new settings and new MCP servers.

The update marks changes made here to `.mcp.json` and to the agent's configuration files with `!`, since they decide what the agent runs. It lists the settings that need `a2a-serve restart`. `a2a-serve status` shows the commit the agent is at.

## Write a recipe

Any directory set up for a2a-serve is already a recipe. Commit the files that define the agent:

- `.a2a/config.yaml` with the agent settings.
- `AGENTS.md`, skills and `.mcp.json`, as for any [project](mcp-and-skills.md).
- `README.md`, which says what the agent does, and which access and variables it needs.
- `.a2a/recipe.yaml`, which describes the MCP servers. It is optional.

The command copies everything except `.git` and `.a2a`, from which it reads only `config.yaml` and `recipe.yaml`. A recipe has a [size limit](../reference/limits.md#recipes).

A relative mount source in `config.yaml` is relative to the agent directory, so `mounts: ["data:/data:ro"]` mounts a directory the recipe ships.

List in `secrets` only what the agent itself needs. Leave the model's API key to the user, unless the recipe pins a model. Pin the version of each MCP server in `.mcp.json`, such as `some-mcp-server@1.2.3`, since a server at `@latest` runs whatever was published last.

To let read-only MCP tools run without asking, allow them by `tool:` name and deny the other kinds:

```yaml
approvals:
  allow: [read, search, think, "tool:grafana.*", "tool:kubernetes.*"]
  deny: [edit, delete, move, execute, fetch]
```

### recipe.yaml

`.a2a/recipe.yaml` describes the MCP servers of `.mcp.json` and their variables for the questions:

```yaml
mcp:
  grafana:
    description: metrics, logs and traces through Grafana
  kubernetes:
    description: read-only state of a Kubernetes cluster
    optional: true
    mounts: [/kube]
vars:
  GRAFANA_URL:
    description: base URL of your Grafana
    secret: false
  GRAFANA_SERVICE_ACCOUNT_TOKEN:
    description: token of a Viewer service account
```

- A server is required unless it is `optional: true`. The command asks whether to use an optional server before it asks for its variables.
- `mounts` lists the mounts, by destination, that only this server uses. They are left out while the server is off.
- `description` appears in the question for a variable. `secret: false` shows the answer as it is typed and saves it as a setting. Other variables are hidden and saved as secrets.

The command records the recipe in `.a2a/recipe.lock` of the new agent. See [Agent Directory](../advanced/agent-directory.md).
