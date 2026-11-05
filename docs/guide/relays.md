# Relays

`a2a-serve relay` runs an [Agent Mesh](../advanced/agent-mesh.md) relay that agents on laptops and in hosted sandboxes join from anywhere.

> [!NOTE]
> Experimental. Agents join relays in builds with `-tags agentmesh` (see [Networks](networks.md#agent-mesh-relays-experimental)). The relay commands work in every build.

A relay admits like a tailnet: everyone it admits can call everyone else it admits. Agents on it find each other by name, call each other from behind NAT, and get public URLs on the relay. Today the relay admits agents you invite. Signing in with Google, limited to an email allowlist, is planned.

## Run a relay

A relay lives in a relay directory, like an agent: settings in `.a2a/relay.yaml`, state in `.a2a/state/relay`. A directory holds either an agent or a relay.

The relay needs an address that doesn't change, since agents and pairing tokens name it. `relay run` serves plain HTTP, so put it behind a reverse proxy or a tunnel that ends TLS, and give that address as `--url`:

```bash
mkdir ~/relay && cd ~/relay
a2a-serve relay run --url https://relay.example.com
```

The first run creates the relay directory, generates the relay's admin token, and downloads the `agentmesh-one` release this a2a-serve pins. It checks the download against a SHA-256 built into a2a-serve. Later runs need no flags:

```bash
a2a-serve relay run              # in the foreground
a2a-serve relay run --service    # as a launchd or systemd user service
```

For a demo, `--tunnel cloudflare` publishes the relay through a Cloudflare quick tunnel, with `cloudflared` from `PATH`. The tunnel's URL changes on every start, and agents have to join again then:

```bash
a2a-serve relay run --tunnel cloudflare
```

A relay keeps its data in SQLite in its relay directory, or in PostgreSQL with `--database`. [`relay run`](../reference/commands/relay.md#a2a-serve-relay-run) lists every flag.

`a2a-serve relay status` shows the relay's address, the versions it runs and how many agents are on it.

## Invite agents

`relay invite` creates a single-use token that joins one agent:

```bash
a2a-serve relay invite ci-bot
```

```
invite ci-bot for https://relay.example.com, one agent, until 2026-11-06 09:00 UTC:
  mesh-bt-72fb0175…
on that agent: a2a-serve config set --private https://relay.example.com --public https://relay.example.com --relay-token -
```

The token is printed once. An unused invite expires after a day, or after `--expires`, up to a week. An invited agent keeps its place on the relay while it is away, even for days, until you remove it. An agent that stays away longer than 30 days needs a new invite.

## Members and removal

```bash
a2a-serve relay members
```

```
AGENT     WHO      JOINED            SEEN              PEER
coder     invite   2026-11-04 10:12  2026-11-05 09:58  12D3KooWQx…
reviewer  invite   2026-11-04 10:15  2026-11-05 09:58  12D3KooWLm…
```

`relay remove` takes an agent name, a peer ID, or an email, which removes every agent of that person. The relay refuses the removed agent's next request, and the other agents drop it:

```bash
a2a-serve relay remove reviewer
```

## Manage a relay elsewhere

On the relay's machine, the relay commands reach the relay through its control socket. From anywhere else, give its URL and admin token. The admin token is in `.a2a/state/relay/secrets` under `RELAY_ADMIN_TOKEN`, unless you set it with `--admin-token`:

```bash
a2a-serve relay invite ci-bot --url https://relay.example.com --admin-token env:RELAY_ADMIN_TOKEN
```

## Upgrades

Each a2a-serve release pins one Agent Mesh version. Agents and the relay should run the same one; `a2a-serve doctor --online` on an agent compares them.

After you install a newer a2a-serve on the relay's machine, run:

```bash
a2a-serve relay upgrade
```

It downloads the new `agentmesh-one` and restarts the relay. A newer Agent Mesh migrates the relay's database, and an older one can't use it again. So the upgrade first backs up a SQLite database to `.a2a/state/relay/backups`, and keeps the last three backups. A relay refuses to start with an older Agent Mesh than the one that last used its data. Back up a PostgreSQL database yourself before you upgrade.

## Run in a container

Each release publishes the image `ghcr.io/a2aproject/a2a-serve-relay`. It runs `a2a-serve relay run` with the pinned `agentmesh-one`, in a relay directory that lasts as long as the container. Give it every setting as an argument, and keep its data in PostgreSQL:

```bash
docker run -p 8080:8080 -e RELAY_ADMIN_TOKEN -e RELAY_DATABASE ghcr.io/a2aproject/a2a-serve-relay \
  --listen=:8080 --url=https://relay.example.com \
  --admin-token=env:RELAY_ADMIN_TOKEN --database=env:RELAY_DATABASE
```

Run one container per relay. Agent Mesh runs a relay as one process, and two would form two meshes behind one URL. A guide for Cloud Run is planned.

## Check a relay

In the relay directory, `a2a-serve doctor` checks the relay's settings, the permissions of its state, the downloaded `agentmesh-one` and the version of its data. With `--online`, it also checks that the relay answers at its URL.
