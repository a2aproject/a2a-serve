# a2a-serve relay

<!-- Generated from the command definitions; do not edit. -->

Run an Agent Mesh relay: agents on laptops and in hosted sandboxes connect
out to it, call each other by name, and get public URLs on it. Everyone the
relay admits can call everyone else it admits.

A relay lives in a relay directory, like an agent: settings in
.a2a/relay.yaml, state in .a2a/state/relay. The other relay commands manage
the relay of the nearest relay directory (or --dir) through its control
socket, or a relay elsewhere with --url and its admin token.

```
a2a-serve relay <command>
```

## a2a-serve relay invite

Create an invite: a single-use token that joins one agent to the relay
without a sign-in, such as an agent on a server. The token is printed once.
An unused invite expires.

An invited agent keeps renewing its membership, even after it slept for days,
until it is removed with `a2a-serve relay remove`. One that stays away
longer than 30 days needs a new invite.

Inviting the name of an invited agent the relay removed lets that agent
back. It joins again with its identity, so it keeps its URLs.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve relay invite <name> [flags]
```

**Examples**

```bash
a2a-serve relay invite ci-bot
a2a-serve relay invite ci-bot --expires 72h
a2a-serve relay invite ci-bot --url https://relay.example.com --admin-token env:RELAY_ADMIN_TOKEN
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--admin-token string` |  | The admin token of the relay at --url, as a secret value: env:VAR, -, or the value |
| `--dir string` |  | Relay directory (default: the nearest directory with .a2a/relay.yaml, walking up from the current one) |
| `--expires duration` | `24h0m0s` | How long the invite stays valid, in whole hours up to 168h |
| `--timeout duration` | `1m0s` | Timeout of each call to the relay, a duration such as 30s or 2m |
| `--url string` |  | Manage the relay at this URL, with --admin-token |

## a2a-serve relay members

List the agents on the relay: the agents they serve, who they joined as (an
email for a signed-in agent, or an invite), when they joined, when they last
reported to the relay, and their peer ID.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve relay members [flags]
```

Aliases: `ls`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--admin-token string` |  | The admin token of the relay at --url, as a secret value: env:VAR, -, or the value |
| `--banned` |  | Also list the removed members |
| `--dir string` |  | Relay directory (default: the nearest directory with .a2a/relay.yaml, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the relay, a duration such as 30s or 2m |
| `--url string` |  | Manage the relay at this URL, with --admin-token |

## a2a-serve relay remove

Remove agents from the relay at once: the agent with that name or peer ID,
or every agent of a person, given their email. The relay refuses their next
request, and the other agents drop them. Removing an agent that signed in
bans the Google account behind it, so the person can't sign in again either.

To let a removed invited agent back, invite its name again with
`a2a-serve relay invite`. It joins again with its identity.

For people who sign in, --allow-email decides: when the relay starts, it
removes the agents of people the list no longer admits, and lets back the
removed people it admits. To keep someone out, also drop them from
--allow-email.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve relay remove <agent|email|peer-id> [flags]
```

Aliases: `rm`

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--admin-token string` |  | The admin token of the relay at --url, as a secret value: env:VAR, -, or the value |
| `--dir string` |  | Relay directory (default: the nearest directory with .a2a/relay.yaml, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the relay, a duration such as 30s or 2m |
| `--url string` |  | Manage the relay at this URL, with --admin-token |

## a2a-serve relay run

Run a relay: one process that serves HTTP on --listen and runs Agent Mesh's
agentmesh-one behind it on loopback, at the version this a2a-serve pins. The
first run creates the relay directory here and downloads agentmesh-one.

The relay needs an address that doesn't change, since agents and pairing
tokens are tied to it. Serve it behind a reverse proxy or a tunnel that ends
TLS, and give that address as --url. --tunnel cloudflare runs a Cloudflare
quick tunnel for a demo; its URL changes on every start.

People sign in to the relay from their agents with Google, limited to
--allow-email, once you register a Google app for the relay and give it with
--google-client-id and --google-client-secret. Agents that can't sign in
join with `a2a-serve relay invite`.

The flags are saved in .a2a/relay.yaml, so later runs need none. A newer
a2a-serve that pins a newer Agent Mesh backs up the relay's database first,
and an older one refuses to run on it.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve relay run [flags]
```

**Examples**

```bash
a2a-serve relay run --url https://relay.example.com
a2a-serve relay run --url https://relay.example.com --allow-email '*@example.com' \
  --google-client-id <id> --google-client-secret env:GOOGLE_CLIENT_SECRET
a2a-serve relay run --tunnel cloudflare
a2a-serve relay run --service
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--admin-token string` |  | The relay's admin token, at least 32 characters, as a secret value: env:VAR, -, or the value (default: generated on the first run) |
| `--allow-email email` |  | People who may sign in to the relay with Google: an email, or \*@domain for the accounts of a Google Workspace domain; replaces the saved list, and none turns sign-in off and forgets the Google app (repeatable; commas also separate values) |
| `--database string` |  | A PostgreSQL DSN, as a secret value: env:VAR, -, or the value (default: SQLite in the relay directory; none goes back to it) |
| `--dir string` |  | Relay directory (default: the nearest relay directory up from the current one, or else the current directory) |
| `--google-client-id string` |  | Client ID of the relay's Google app, which people sign in with (none removes the app) |
| `--google-client-secret string` |  | Client secret of the relay's Google app, as a secret value: env:VAR, -, or the value |
| `--listen string` |  | Address to serve HTTP on (default 127.0.0.1:8443) |
| `--service` |  | Install and start the relay as a launchd or systemd user service |
| `--tunnel string` |  | Publish the relay through a Cloudflare quick tunnel (cloudflare, or none); needs cloudflared on PATH |
| `--url string` |  | The relay's address, which agents join: https://..., or http:// on loopback |

## a2a-serve relay status

Show the relay's address, whether it runs, the versions of a2a-serve and
Agent Mesh it runs and this a2a-serve pins, who can join, how many agents are
on it, and its last backup before an upgrade. A stopped local relay shows
what its relay directory says.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve relay status [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--admin-token string` |  | The admin token of the relay at --url, as a secret value: env:VAR, -, or the value |
| `--dir string` |  | Relay directory (default: the nearest directory with .a2a/relay.yaml, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the relay, a duration such as 30s or 2m |
| `--url string` |  | Manage the relay at this URL, with --admin-token |

## a2a-serve relay upgrade

Upgrade the local relay to the Agent Mesh version this a2a-serve pins. Run it
after you install a newer a2a-serve. It downloads the new agentmesh-one, then
restarts a running relay, or upgrades the data of a stopped one.

A newer Agent Mesh migrates the relay's database, and an older one can't use
it again, so the upgrade backs up a SQLite database first and keeps the last
three backups. Back up a PostgreSQL database yourself before you upgrade. A
relay elsewhere upgrades when it runs the new a2a-serve.

```
a2a-serve relay upgrade [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Relay directory (default: the nearest directory with .a2a/relay.yaml, walking up from the current one) |
| `--timeout duration` | `3m0s` | How long to wait for the relay to restart, a duration such as 30s or 2m |
