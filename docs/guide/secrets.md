# Secrets

The server stores API keys and tokens for you, so an agent in the background doesn't need your shell's environment.

## API keys for the agent

`--secret NAME` passes an environment variable to the agent, such as the API key of its model:

```sh
export OPENROUTER_API_KEY=sk-or-…
a2a-serve start opencode --model openrouter/~google/gemini-flash-latest --secret OPENROUTER_API_KEY
```

The command reads the variable from your environment, or asks for it with a hidden prompt. It stores the value in `.a2a/state/secrets` and puts only the name in `config.yaml`. Later starts, restarts and services use the stored value. `start` and `config set` both take `--secret`, and it repeats.

opencode uses `OPENROUTER_API_KEY` only with an OpenRouter model, so pass `--model` too. `start`, `config set` and `doctor` warn when no secret holds the key of the model's provider.

`--secret NAME=value` takes the value in any of the [forms](#value-forms) below, such as `--secret GITHUB_TOKEN=env:GH_TOKEN`. `NAME=secret:OTHER` passes the stored value of `OTHER` under another name.

| Task | Command |
| --- | --- |
| Add a key | `a2a-serve config set --secret GITHUB_TOKEN` |
| Change a key | Set the variable to the new value and run `config set --secret NAME` again. |
| Remove a key | `a2a-serve config unset secret GITHUB_TOKEN` |
| See which keys are stored | `a2a-serve config show` |

A changed key reaches the agent after `a2a-serve restart`, or with `--restart` on `config set`.

The [sandbox](sandbox.md#what-each-callers-container-gets) decides which callers' containers get the keys. `${VAR}` in `.mcp.json` takes its value from them too ([MCP and Skills](mcp-and-skills.md)).

> [!WARNING]
> A tricked agent, and any caller who can approve commands, can read the keys that their container gets. [Keep keys out of the sandbox](#keep-keys-out-of-the-sandbox), or give guests and members you trust less a [separate key](sandbox.md#give-guests-a-key-of-their-own). See [Security](../advanced/security.md#the-sandbox).

## Keep keys out of the sandbox

The server can add a key to the agent's requests as they leave the Docker sandbox, so the key never enters the container. A tricked agent then has no key to send elsewhere.

For the key of a model provider:

```console
$ export OPENROUTER_API_KEY=sk-or-…
$ a2a-serve start opencode --model openrouter/~google/gemini-flash-latest \
    --inject-model-key openrouter=OPENROUTER_API_KEY
```

`--inject-model-key` knows `anthropic`, `gemini`, `openai` and `openrouter`.

For any other HTTPS API, such as another provider or a company gateway, name the host and the header. Quote the rule in single quotes, so that the shell keeps `$GITHUB_TOKEN` for the server to read:

```console
$ export GITHUB_TOKEN=ghp_…
$ a2a-serve config set --inject-header 'api.github.com=Authorization: Bearer $GITHUB_TOKEN' --restart
```

The command stores the value like `--secret` does. In the agent's environment the variable holds a placeholder that works only on that host, so clients that need the variable start as usual. For these hosts the server ends the container's TLS with a certificate of its own, adds the header and forwards the request ([how it works](../advanced/security.md#the-sandbox)).

`a2a-serve status` lists the hosts with the names of their secrets:

```console
$ a2a-serve status
...
  inject   openrouter.ai gets OPENROUTER_API_KEY; api.github.com gets GITHUB_TOKEN
```

| Task | Command |
| --- | --- |
| Change a key | Set the variable to the new value and run the same `config set --inject-…` command with `--restart`. |
| Remove a rule | `a2a-serve config unset inject-header api.github.com` or `a2a-serve config unset inject-model-key openrouter` |

Rules apply at the next start. [Sandbox rules](sandbox.md#rules-for-several-callers) decide which callers' containers get the key. With `--egress allowlist`, a rule also allows its host.

The rules need the Docker sandbox with the default network rules. `config set` refuses them with `--sandbox local` and `--egress unrestricted`.

What the rules don't protect:

- The agent can still spend the key on its host. Give the agent a key with a spending limit.
- A host that echoes request headers back shows the key to the agent.
- A client that pins certificates fails on the hosts that get headers.

## Tokens for the server

Flags that give the server a token or password, such as `messenger add --token`, `config set --tailscale-authkey` or `config set --voice-key`, store the value in `state/secrets` too. The agent never sees these values.

### Value forms

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
