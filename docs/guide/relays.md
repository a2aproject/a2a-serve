# Relays

`a2a-serve relay` runs an [Agent Mesh](../advanced/agent-mesh.md) relay that agents on laptops and in hosted sandboxes join from anywhere.

> [!NOTE]
> Experimental. Agents join relays in builds with `-tags agentmesh` (see [Networks](networks.md#agent-mesh-relays-experimental)). The relay commands work in every build.

A relay admits like a tailnet: everyone it admits can call everyone else it admits. Agents on it find each other by name, call each other from behind NAT, and get public URLs. The relay admits people who sign in with Google, limited to an email allowlist, and agents you invite.

## Run a relay

The relay needs an address that doesn't change. `relay run` serves plain HTTP, so put it behind a reverse proxy or a tunnel that ends TLS, and give that address as `--url`. The proxy sets `X-Forwarded-For` ([relay routes](../reference/http-routes.md#relay-routes)).

```bash
mkdir ~/relay && cd ~/relay
a2a-serve relay run --url https://relay.example.com
```

The first run creates the relay directory and downloads `agentmesh-one`. Later runs need no flags:

```bash
a2a-serve relay run              # in the foreground
a2a-serve relay run --service    # as a launchd or systemd user service
```

For a demo, `--tunnel cloudflare` publishes the relay through a Cloudflare quick tunnel with a new URL on every start.

[`relay run`](../reference/commands/relay.md#a2a-serve-relay-run) lists every flag, such as `--database` for PostgreSQL. `a2a-serve relay status` shows the relay's address and agent count.

## Let people sign in

The relay signs people in with Google and admits the accounts that match `--allow-email`. An entry is an email or `*@example.com`, with the [same rules as for agents](email-sign-in.md). Register the relay's Google app once:

1. In the Google Cloud console, create an OAuth client of type **Web application**.
2. Add `https://relay.example.com/a2a-serve/signin/callback` as an authorized redirect URI. `a2a-serve doctor` in the relay directory prints it.
3. Publish the app, as **Internal** for a Workspace domain or **In production** otherwise. Google expires the sign-ins of an app in testing after 7 days.

Give the app and the allowlist to the relay:

```bash
a2a-serve relay run --url https://relay.example.com --allow-email '*@example.com' \
  --google-client-id <id> --google-client-secret env:GOOGLE_CLIENT_SECRET
```

Sign-in doesn't work with `--tunnel cloudflare`. `--allow-email` replaces the saved list, and `--allow-email none` turns sign-in off.

An agent joins without a token:

```bash
a2a-serve start opencode --private https://relay.example.com --public https://relay.example.com
```

```
→ sign in to the relay https://relay.example.com: https://relay.example.com/a2a-serve/signin/device?link=… (code BCDF-GHJK)
```

Open the link and sign in with Google. On another device, open `https://relay.example.com/a2a-serve/signin/device` and type the code. The agent stays signed in across restarts.

## Invite an agent

`relay invite` creates a single-use token for a server that can't open a browser:

```bash
a2a-serve relay invite ci-bot
```

```
invite ci-bot for https://relay.example.com, one agent, until 2026-11-06 09:00 UTC:
  mesh-bt-72fb0175…
on that agent: a2a-serve config set --private https://relay.example.com --public https://relay.example.com --relay-token -
on a new agent: a2a-serve config set --agent <agent> --private https://relay.example.com --public https://relay.example.com --relay-token -
```

The token is printed once. See [limits](../reference/limits.md#relay) for how long an invite lasts. To revoke an unused invite, such as one that leaked into a log, run `a2a-serve relay remove ci-bot`.

## Members and removal

```bash
a2a-serve relay members
```

```
AGENT     WHO               JOINED                         SEEN              PEER
coder     anna@example.com  2026-11-04 10:12               2026-11-05 09:58  12D3KooWQx…
reviewer  invite reviewer   2026-11-04 10:15               2026-11-05 09:58  12D3KooWLm…
-         invite ci-bot     unused until 2026-11-06 09:00  -                 -
```

`relay remove` takes an agent name, a peer ID or an email. An email removes every agent of that person:

```bash
a2a-serve relay remove reviewer
```

Removing an agent that signed in also bans its Google account, so the person's other agents lose their place too. The removed agent's `a2a-serve status` reports the removal.

`--allow-email` decides for people who sign in. When the relay starts, it removes the agents of people the list no longer admits and lets back the ones it admits again. To keep one person out of a `*@example.com` domain, list the others by email. To let a removed invited agent back, invite its name again:

```bash
a2a-serve relay invite reviewer
```

`relay members --banned` lists removed agents.

## Manage a relay elsewhere

From another machine, give the relay's URL and admin token (`RELAY_ADMIN_TOKEN` in `.a2a/state/relay/secrets`, or the value of `--admin-token`):

```bash
a2a-serve relay invite ci-bot --url https://relay.example.com --admin-token env:RELAY_ADMIN_TOKEN
```

## Upgrade

Each a2a-serve release pins one Agent Mesh version. Agents and the relay should run the same one; `a2a-serve doctor --online` on an agent compares them. After you install a newer a2a-serve on the relay's machine, run:

```bash
a2a-serve relay upgrade
```

It downloads the new `agentmesh-one`, backs up a SQLite database to `.a2a/state/relay/backups`, and restarts the relay. Back up a PostgreSQL database yourself before you upgrade.

## Run in a container

Each release publishes the image `ghcr.io/a2aproject/a2a-serve-relay`. Give it every setting as an argument, and keep its data in PostgreSQL:

```bash
docker run -p 8080:8080 -e RELAY_ADMIN_TOKEN -e RELAY_DATABASE ghcr.io/a2aproject/a2a-serve-relay \
  --listen=:8080 --url=https://relay.example.com \
  --admin-token=env:RELAY_ADMIN_TOKEN --database=env:RELAY_DATABASE
```

Run one container per relay. [Relay on Cloud Run](../../examples/05-relay-cloud-run/README.md) runs the image on Google Cloud.

## Check a relay

In the relay directory, `a2a-serve doctor --online` checks the settings and that the relay answers at its URL. The relay's machine has to reach that URL.
