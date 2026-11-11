# Running

Each agent lives in its project directory and runs as its own process, with its own workspace, messengers, network identity and pairings. To run several agents on one machine, start one in each directory. They find each other on the private network the same way agents on different machines do.

`a2a-serve` commands find the agent the way git finds a repository, or take `--dir <path>`. The [agent directory](../advanced/agent-directory.md) describes what the server keeps there.

> [!WARNING]
> A project's `config.yaml` and `.mcp.json` decide what runs on your machine. Read them before you start an agent in a repository you didn't write. See [Security](../advanced/security.md#what-a-project-can-make-your-machine-run).

| Command | What it does |
| --- | --- |
| `a2a-serve start` | Starts the agent in the foreground. |
| `a2a-serve start --detach` | Starts it in the background. |
| `a2a-serve start --service` | Installs and starts it as a systemd (Linux) or launchd (macOS) user service. |
| `a2a-serve status` | Shows URLs, messengers, owner links and task counts. |
| `a2a-serve status --url` | Prints only the URL that reaches furthest: public, then private, then local. `--url=local\|private\|public` picks one. |
| `a2a-serve card` | Prints the agent card as callers see it. `--url=local\|private\|public` picks the URL. |
| `a2a-serve logs [-f]` | Shows the log. |
| `a2a-serve stop [--uninstall]` | Stops it. `--uninstall` also removes the service. |
| `a2a-serve restart` | Stops it and starts it again in the same mode. Starts a stopped agent. |

Commands that only change stored data, such as `pair`, `contact add --url`, `schedule add` and `messenger add`, also work while the agent is stopped. The agent picks up the changes on its next start.

To restart an agent that runs in the foreground of another terminal, press Ctrl-C there and run `a2a-serve start` again.

A stop fails the tasks that still run, with a message to send them again.
