# The a2a CLI

a2a-serve works with the [a2a CLI](https://github.com/a2aproject/a2a-cli), the command-line A2A client of the A2A project. People and nodes call agents with it, the agent calls other agents with it, and it can run a2a-serve as a [command plugin](../guide/a2a-command-plugin.md).

`a2a-serve doctor` checks that the installed `a2a` is recent enough and prints the command to upgrade it. `a2a-serve doctor --fix` does the setup on this page.

## How a2a-serve uses the a2a CLI

### Calls from the agent

The agent calls other agents with `a2a send`. The server sets these variables for it:

| Variable | Value |
| --- | --- |
| `A2ACLI_UNIX_SOCKET` | The server's control socket. The `a2a` CLI sends every request through it, whatever the host of the URL. Set on Linux and with `--sandbox local`. |
| `A2ACLI_CREDENTIAL_HELPER` | `host.docker.internal=<path>` on Docker Desktop, where the agent reaches the server over a TCP bridge. The helper script prints the bridge key, and only for that host. |
| `A2ACLI_TIMEOUT` | `5m`, unless `--env` sets it. |

The agent images carry the `a2a` version that this a2a-serve needs. With `--sandbox local`, the agent uses the `a2a` on your `PATH`.

### Tokens for callers

`a2a-serve credential` is a credential helper in the [Bazel credential helper protocol](https://github.com/EngFlow/credential-helper-spec). For a URL, it returns:

- the token that `a2a-serve pair join` saved in `~/.config/a2a-serve/credentials.json` for the longest matching agent URL, or
- `$A2A_NODE_TOKEN`, for requests a webhook node sends to its agent.

The `a2a` CLI runs it when it is listed in `~/.config/a2a-cli/config.yaml`. `a2a-serve doctor --fix` adds it. To add it by hand:

```bash
mkdir -p ~/.config/a2a-cli
grep -q '^credential-helper:' ~/.config/a2a-cli/config.yaml 2>/dev/null ||
  printf 'credential-helper:\n  - command: [a2a-serve, credential]\n' >> ~/.config/a2a-cli/config.yaml
```

### Served files

| Route | Content |
| --- | --- |
| `GET /skill.md` | The output of `a2a skill`, for agents that call this one. |
| `GET /download/a2a` | The `a2a` binary next to the server, for webhook nodes. |

### Version check

`a2a-serve doctor` reads `a2a version -o json` and fails on versions older than the one this a2a-serve needs. Those versions lack `--unix-socket` or the credential helper format above, so their calls get a 401. With `--sandbox local`, the server also logs a warning on start.
