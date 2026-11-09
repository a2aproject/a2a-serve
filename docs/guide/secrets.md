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
> A tricked agent or any caller who can approve commands can read the keys that their container gets. [Keep keys out of the sandbox](#keep-keys-out-of-the-sandbox), or give guests and members you trust less a [separate key](sandbox.md#give-guests-a-key-of-their-own). See [Security](../advanced/security.md#the-sandbox) and [Access Model](../advanced/access-model.md#agent-processes).

## Keep keys out of the sandbox

The server can add a key to the agent's requests as they leave the Docker sandbox, so the key never enters the container. A tricked agent then has no key to send elsewhere.

For the key of a model provider:

```console
$ export OPENROUTER_API_KEY=sk-or-…
$ a2a-serve start opencode --model openrouter/~google/gemini-flash-latest \
    --inject-model-key openrouter=OPENROUTER_API_KEY
```

For any other HTTPS API, name the host and the header. Quote the rule in single quotes, so that the shell keeps `$GITHUB_TOKEN` for the server to read:

```console
$ export GITHUB_TOKEN=ghp_…
$ a2a-serve config set --inject-header 'api.github.com=Authorization: Bearer $GITHUB_TOKEN' --restart
```

1. The command reads each `$NAME` from your environment, or else uses the secret the agent already has. It stores the value in `state/secrets` and adds the name to `secrets:`, as `--secret` does.
2. In the agent's environment and in the variables of `.mcp.json`, the variable holds a placeholder, so that clients that need it start as usual. The placeholder works only on the rule's host.
3. The server answers the container's HTTPS connections to the host with a certificate of its own, adds the header to each request and sends the request on to the real host. The container trusts the server's certificate authority through `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS` and the image's system bundle. Connections to other hosts pass through unchanged.

`--inject-model-key` knows these providers:

| Provider | Host | Variables the agents read |
| --- | --- | --- |
| `anthropic` | `api.anthropic.com` | `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_AUTH_TOKEN` |
| `gemini` | `generativelanguage.googleapis.com` | `GOOGLE_GENERATIVE_AI_API_KEY`, `GEMINI_API_KEY` |
| `openai` | `api.openai.com` | `OPENAI_API_KEY` |
| `openrouter` | `openrouter.ai` | `OPENROUTER_API_KEY` |

The server adds a model key only to requests for completions, messages, embeddings and the model list. The agent can't list, download or delete the files and batches stored with the key, or create keys. A request to the inference endpoint can still name a stored file by its ID. For another provider or a company gateway, use `--inject-header` with its host.

`a2a-serve status` lists the hosts with the names of their secrets:

```console
$ a2a-serve status
...
  inject   openrouter.ai gets OPENROUTER_API_KEY; api.github.com gets GITHUB_TOKEN
```

| Task | Command |
| --- | --- |
| Change a key | Set the variable to the new value and run the same `config set --inject-…` command with `--restart`. |
| Remove a rule | `a2a-serve config unset inject-header api.github.com` or `a2a-serve config unset inject-model-key openrouter`. The secret is removed with the rule, so it doesn't reach the agent's environment. |

Rules apply at the next start, so run `a2a-serve restart` or pass `--restart`. With `--egress allowlist`, a rule also allows its host for the containers that get its secrets.

Sandbox rules decide which containers get the key. A container that a rule doesn't give `GITHUB_TOKEN` gets neither the variable nor the header. `NAME=secret:OTHER` adds the value of `OTHER`.

The rules need the egress boundary. `config set` refuses them with `--sandbox local` and `--egress unrestricted`. Where the boundary can't apply, such as with an a2a-serve built with cgo or on Docker Desktop without the egress container image, the agent doesn't start, and the error names the secrets.

What the rules don't protect:

- The agent can still spend the key on its host, through the requests the server lets pass. Give the agent a key with a spending limit of its own.
- A host that echoes request headers back shows the key to the agent.
- A client that pins certificates, or that ignores the system bundle and both variables, fails on the hosts that get headers.

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
