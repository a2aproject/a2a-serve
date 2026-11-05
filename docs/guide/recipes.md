# Recipes

A recipe is a directory with a ready-made agent, and `a2a-serve recipe` creates an agent from one with a single command.

## Create an agent from a recipe

A recipe is a directory with a committed `.a2a/config.yaml`. It may also hold `AGENTS.md`, skills, `.mcp.json` and `.a2a/recipe.yaml`. Any directory set up for a2a-serve is already a recipe. Its version is a git ref.

```sh
a2a-serve recipe https://github.com/acme/agents/tree/v1.2/experts/elk --name elk
cd elk
a2a-serve start
```

The source can be:

- a tree URL in the GitHub layout, on any host: `https://<host>/<org>/<repo>/tree/<ref>/<path>`;
- a repository URL, for the default branch and the repository root;
- a git URL with an optional `#<ref>:<path>`, such as `git@github.com:acme/agents.git#v1.2:experts/elk`;
- a local directory.

The command fetches the recipe with your `git`, so your SSH keys, credential helpers and `insteadOf` rules apply. It refuses `http://` and `git://` URLs, and URLs with credentials in them.

The command then:

1. Shows what the agent will run: the agent, the sandbox, the mounts, the MCP servers, the skills and the approvals. It marks risky entries with `!`.
2. Asks before it creates the agent in `./<name>`, or in `--dir`. `--yes` skips the question, and `--dry-run` only prints the review.
3. Asks for the secrets the agent lacks, and whether to use each optional MCP server. A value set in your shell is offered first.
4. Saves the agent. Start it with `a2a-serve start`.

Without a terminal, pass `--yes` and give the values with `--secret`, `--env` or the environment. An optional MCP server without its values stays off. To create an agent without a server it requires, add `--disable-mcp <server>`.

The flags of `a2a-serve config set` apply on top of the recipe, such as `--memory 8g` or `--sandbox local`. See the [command reference](../reference/commands/recipe.md) for every flag.

Updating an agent from a newer version of its recipe is planned.

## What a recipe sets

A recipe sets what the agent is: the agent, its command and image, the sandbox, mounts, secrets, environment, description, approvals and limits. You decide where the agent is reachable and who may call it. The command leaves out the recipe's port, workspace, networks, access settings, messengers, backups and Google sign-in, and the review lists the ones it left out. Add messengers and networks afterwards, as for any agent:

```sh
a2a-serve channel add telegram --token env:TELEGRAM_BOT_TOKEN
a2a-serve config set --private tailscale
```

An agent that queries your systems acts with the credentials you give it, for every caller. Give it credentials limited to the data it needs. A recipe's README says which access it needs.

## Write a recipe

Commit the files that define the agent:

- `.a2a/config.yaml` with the agent settings. `a2a-serve start` adds a port to it on the first start, which the recipe command leaves out.
- `AGENTS.md`, skills and `.mcp.json`, as for any [project](mcp-and-skills.md).
- `README.md`, which says what the agent does, and which access and variables it needs.
- `.a2a/recipe.yaml`, which describes the MCP servers. It is optional.

The command copies everything except `.git` and `.a2a`, from which it reads only `config.yaml` and `recipe.yaml`. Symlinks must be relative and stay inside the recipe. A recipe holds at most 10,000 files and 100 MB.

`config.yaml` may only use settings the user's a2a-serve knows. An older a2a-serve refuses a recipe with newer settings, and asks the user to upgrade.

List in `secrets` only what the agent itself needs. The command asks for the variables of `.mcp.json` per server. Leave the model's API key to the user, unless the recipe pins a model: the command asks for one when `secrets` names none.

Pin the version of each MCP server in `.mcp.json`, such as `some-mcp-server@1.2.3`. A server at `@latest` runs whatever was published last.

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
    description: base URL of your Grafana, such as https://grafana.example.com
    secret: false
  GRAFANA_SERVICE_ACCOUNT_TOKEN:
    description: token of a Grafana service account with the Viewer role
```

- Every server is required unless it is `optional: true`. The command asks whether to use an optional server before it asks for its variables. A server the user declines is turned off with `--disable-mcp`.
- `mounts` names, by destination, the mounts of `config.yaml` that only this server uses. They are left out while the server is off. An optional server whose mount source doesn't exist stays off.
- A variable's `description` appears in its question. `secret: false` shows the answer as it is typed and saves it with `--env`. Other variables are hidden and saved as secrets.

The file may only name servers of `.mcp.json`, mounts of `config.yaml`, and variables of `.mcp.json` or of `secrets`. Without the file, every server is required.

The command records the recipe in `.a2a/recipe.lock` of the new agent. See [Agent Directory](../advanced/agent-directory.md).
