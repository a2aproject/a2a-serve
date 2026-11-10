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
| `a2a-serve stop [--uninstall]` | Stops it, and waits until it exits. A server that doesn't answer gets SIGTERM, then SIGKILL after 10 seconds. `--uninstall` also removes the service. |
| `a2a-serve restart` | Stops it and starts it again in the same mode. Starts a stopped agent. |

Commands that only change stored data, such as `pair`, `contact add --url`, `contact refresh`, `contact rm`, `schedule add`, `messenger add` and `messenger rm`, also work while the agent is stopped. It picks up the changes on its next start. While the agent starts or stops, these commands fail with exit code 8; run them again once `a2a-serve status` shows it running or stopped.

`start --detach` and `start --service` return once the agent serves. If the server stops with an error first, they fail and show the end of the log. A service that failed this way stays installed, and the service manager keeps restarting it until you run `a2a-serve stop --uninstall`.

`start --service` fails while the agent runs without a service, in the background or in a terminal. Stop it with `a2a-serve stop`, then run `a2a-serve start --service`.

`restart` checks the settings first, so a misconfigured agent keeps running. A stopped agent starts as a user service if one is installed, and in the background otherwise. To restart an agent running in the foreground of another terminal, press Ctrl-C there and run `a2a-serve start` again.

A stop fails the tasks that still run, with a message to send them again. Callers that follow such a task get this failed status as the final event. The server also stops when its `.a2a/state` directory is deleted.
