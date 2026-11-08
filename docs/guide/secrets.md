# Secrets

The server keeps API keys and tokens in `.a2a/state/secrets`, so an agent running in the background doesn't need your shell's environment. `config.yaml` names the secrets and never holds their values.

## API keys for the agent

`--secret NAME` passes an environment variable to the agent, such as the API key of its model:

```sh
export OPENROUTER_API_KEY=sk-or-…
a2a-serve start opencode --model openrouter/~google/gemini-flash-latest --secret OPENROUTER_API_KEY
```

1. The command reads `OPENROUTER_API_KEY` from your environment. When the variable isn't set, it asks for the value with a hidden prompt on a terminal and fails otherwise.
2. It stores the value in `state/secrets` (mode 0600) and adds the name to `secrets:` in `config.yaml`.
3. On every start, the server sets `OPENROUTER_API_KEY` in the agent's environment, inside the sandbox.

opencode uses `OPENROUTER_API_KEY` only with an OpenRouter model, so pass `--model` too. Without one, `start`, `config set` and `doctor` print a warning.

When no secret holds the key of the model's provider, the same commands warn, and each task fails with the name of the key to set. Without `--model`, opencode uses its free default model. When that model refuses requests, the agent sends nothing, and the server fails the task after 10 minutes.

Your shell needs the variable only when you run the command. Later starts, restarts and services use the stored value.

`--secret` takes the name of the variable, and the agent sees the same name. `start` and `config set` both take it, and it repeats: `--secret OPENROUTER_API_KEY --secret GITHUB_TOKEN`.

`--secret NAME=value` sets the value in any of the [forms](#value-forms) the server's tokens take. `--secret OPENROUTER_API_KEY=-` reads it from a prompt or stdin, `--secret GITHUB_TOKEN=env:GH_TOKEN` reads another variable, and `--secret KEY=secret:OLD_KEY` copies a stored value.

| Task | Command |
| --- | --- |
| Add a key | `a2a-serve config set --secret GITHUB_TOKEN` |
| Change a key | Set the variable to the new value and run `config set --secret NAME` again. |
| Remove a key | `a2a-serve config unset secret GITHUB_TOKEN` removes the name and the stored value. |
| See which keys are stored | `a2a-serve config show` |

A changed key reaches the agent after `a2a-serve restart`, or with `--restart` on `config set`.

`${VAR}` in the project's `.mcp.json` also takes its value from the keys that a conversation's container gets. See [MCP and Skills](mcp-and-skills.md).

Agent containers get the keys passed with `--secret` as the [sandbox defaults](sandbox.md#what-each-callers-container-gets) say. Sandbox rules set them for chosen callers, or pass a stored secret under another name with `NAME=secret:OTHER`. A rule may name only secrets in `secrets`.

> [!WARNING]
> A tricked agent or any caller who can approve commands can read the keys that their container gets. Give guests and other callers you trust less a [separate key](sandbox.md#give-guests-a-key-of-their-own). See [Security](../advanced/security.md#the-sandbox) and [Access Model](../advanced/access-model.md#agent-processes).

## Tokens for the server

Flags that give the server a token or password, such as `messenger add --token` or `config set --tailscale-authkey`, store the value in `state/secrets` too. The agent never sees these values.

### Value forms

They accept these forms:

| Form | Meaning |
| --- | --- |
| `env:VAR` | The value of the environment variable `VAR`. |
| `-` | A hidden prompt on a terminal, or else all of stdin. |
| `secret:NAME` | A value already stored in `state/secrets`. |
| anything else | The value itself. |

`-` keeps tokens out of your shell history:

```sh
a2a-serve messenger add telegram --token -
pass show coder/bot | a2a-serve messenger add discord --token -
```

`pair invite --webhook-header` takes a whole header, which may end in `env:VAR` or `-`: `"Authorization: Bearer env:TOKEN"`.

[Backups](backups.md) include the stored secrets, encrypted with the backup key.
