# Relays

`a2a-serve relay` runs an [Agent Mesh](../advanced/agent-mesh.md) relay that agents on laptops and in hosted sandboxes join from anywhere.

> [!NOTE]
> Experimental. Agents join relays in builds with `-tags agentmesh` (see [Networks](networks.md#agent-mesh-relays-experimental)). The relay commands work in every build.

A relay admits like a tailnet: everyone it admits can call everyone else it admits. Agents on it find each other by name, call each other from behind NAT, and get public URLs on the relay. An agent with `--private-allow-outgoing everyone` calls the others by name without `contact add`. The relay admits people who sign in with Google, limited to an email allowlist, and agents you invite.

## Run a relay

A relay lives in a relay directory, like an agent: settings in `.a2a/relay.yaml`, state in `.a2a/state/relay`. A directory holds either an agent or a relay.

The relay needs an address that doesn't change, since agents and pairing tokens name it. `relay run` serves plain HTTP, so put it behind a reverse proxy or a tunnel that ends TLS, and give that address as `--url`. Run the proxy on the relay's machine or private network, and have it set `X-Forwarded-For`, which the relay uses to [limit each client](../reference/http-routes.md#relay-routes):

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

## Let people sign in

People sign in to the relay from their agents with Google. The relay checks each account against `--allow-email`, with the rules agents use: an entry is an email, or `*@example.com`, which matches only accounts of that Google Workspace domain.

The relay signs people in with its own Google app, which you register once:

1. In the Google Cloud console, create an OAuth client of type **Web application**.
2. Add `https://relay.example.com/a2a-serve/signin/callback` as an authorized redirect URI, with your relay's URL. `a2a-serve doctor` in the relay directory prints it.
3. Publish the app, as **Internal** for a Workspace domain or **In production** otherwise. Google expires the sign-ins of an app in testing after 7 days, and agents then have to sign in again.

Give the app and the allowlist to the relay:

```bash
a2a-serve relay run --url https://relay.example.com --allow-email '*@example.com' \
  --google-client-id <id> --google-client-secret env:GOOGLE_CLIENT_SECRET
```

Sign-in needs a URL that doesn't change, since the Google app names it, so it doesn't work with `--tunnel cloudflare`. `--allow-email` replaces the saved list, and `--allow-email none` turns sign-in off. Restart the relay to change them. The restart applies the new list to agents already on the relay, as [Members and removal](#members-and-removal) explains.

An agent joins without a token:

```bash
a2a-serve start opencode --private https://relay.example.com --public https://relay.example.com
```

```
→ sign in to the relay https://relay.example.com: https://relay.example.com/a2a-serve/signin/device?link=… (code BCDF-GHJK)
```

Open the link, sign in with Google, and approve the sign-in if the page shows the code the agent printed. On another device, open `https://relay.example.com/a2a-serve/signin/device` and type the code. The agent keeps a refresh token, so it doesn't need you again when it renews its credential or restarts. Each renewal checks the account with Google and against the allowlist again. An agent prints a new link when its sign-in no longer works: after you revoke the relay's access in your Google account, after the relay gets a new admin token, or after you move the agent to another relay. The agent sends its refresh token only to the relay that issued it.


`relay invite` creates a single-use token that joins one agent:

```bash
a2a-serve relay invite ci-bot
```

```
invite ci-bot for https://relay.example.com, one agent, until 2026-11-06 09:00 UTC:
  mesh-bt-72fb0175…
on that agent: a2a-serve config set --private https://relay.example.com --public https://relay.example.com --relay-token -
on a new agent: a2a-serve config set --agent <agent> --private https://relay.example.com --public https://relay.example.com --relay-token -
```

The name has up to 64 letters, digits, dots, hyphens or underscores, and starts with a letter or digit. The token is printed once. An unused invite expires after a day, or after `--expires`, up to a week. To revoke it sooner, such as after it leaked into a log, run `a2a-serve relay remove ci-bot`. An invited agent keeps its place on the relay while it is away, even for days, until you remove it. An agent that stays away longer than 30 days needs a new invite.

The agent joins the relay before it starts. When the relay refuses the invite, such as one that is truncated, used, expired or revoked, the start fails with the reason and the next step. An agent that joined before keeps its place: it starts with its saved credential and prints the reason. `a2a-serve doctor` reports the refused invite until the relay accepts one.

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

`WHO` shows the email of an agent that signed in, or the name of the invite it joined with. The unused invites come last.

`relay remove` takes an agent name, a peer ID, or an email, which removes every agent of that person. The relay refuses the removed agent's next request, and the other agents drop it. A name also revokes the unused invites of that name:

```bash
a2a-serve relay remove reviewer
```

Removing an agent that signed in bans the Google account behind it, so the person's other agents lose their place too, and they can't sign in again until the relay lets them back.

The removed agent's server logs the removal. On that agent, `a2a-serve status` and `a2a-serve doctor` report it, and `status` leaves out the private and public URLs.

For people who sign in, `--allow-email` decides. When the relay starts, it removes the agents of people the list no longer admits, and lets back removed people it admits. So dropping a person from the list takes effect with the restart that applies it, and adding them back undoes a removal. To keep someone out, drop them from `--allow-email` too. A `*@example.com` entry admits everyone of that domain, so to keep out one person of it, list the others by email.

To let a removed invited agent back, invite its name again:

```bash
a2a-serve relay invite reviewer
```

The agent comes back with its identity when it restarts. The other agents on the relay reach it again within about a minute and a half. It keeps its URLs, so pairings and contacts that saved them keep working. The relay remembers the names of removed agents in its relay directory, so a relay in a container forgets them when the container goes away. `relay members --banned` lists the removed agents.

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

Run one container per relay. Agent Mesh runs a relay as one process, and two would form two meshes behind one URL. [Relay on Cloud Run](../../examples/05-relay-cloud-run/README.md) runs the image on Google Cloud.

The relay's sign-in keys derive from its admin token, so a container keeps its signed-in agents across restarts when it gets the same admin token.

## Check a relay

In the relay directory, `a2a-serve doctor` checks the relay's settings, the permissions of its state, the downloaded `agentmesh-one` and the version of its data. With `--online`, it also checks that the relay answers at its URL, and that it serves its sign-in there.

On an agent, `a2a-serve status`, `a2a-serve peers` and `a2a-serve doctor` report a lost connection to the relay. The server reconnects when the relay is back.

The relay reads its own sign-in at its URL when it starts, as `agentmesh-one` does. So the machine that runs the relay has to reach the relay's URL, and `relay run` fails with a reason when it can't.
