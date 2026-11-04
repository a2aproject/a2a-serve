# Upgrades

Upgrade `a2a-serve` like any binary, then restart each agent with
`a2a-serve restart` (or `a2a-serve start` in its directory). An agent picks up the
new version on its next start.

## Stored state migrates forward

- **`server.db`** carries its schema version in SQLite's `PRAGMA user_version`. On start, the server applies every migration from that
  version to the current one, each in its own transaction, so a failed
  migration leaves the database as it was. Released migrations never change;
  new versions add new ones.
- **`config.yaml`** settings that are renamed or removed are listed in the
  release notes, and old ones are not read.
- **Renamed commands, routes and variables** are replaced without aliases:
  the release notes name the new form.

## Going back isn't supported

A database migrated by a newer version can't be used by an older one: the
older server refuses to start with "the database is at version N, newer than
this a2a-serve supports". To be able to go back, take a snapshot before upgrading,
and restore it into the agent directory after downgrading:

```sh
a2a-serve backup now                 # with --backup set, or copy .a2a/ while the agent is stopped
```

## Agent images

The agent image tag names the `a2a` CLI version the server needs
(`ghcr.io/a2aproject/harness-<harness>:a2a-<version>`), so a new `a2a-serve`
that needs a newer `a2a` pulls a new image on its next start. The agent's
home in `state/home` is kept, with its sessions and config.

## The a2a CLI

Callers and nodes use the `a2a` CLI. `a2a-serve doctor` checks that the
installed version has what this `a2a-serve` needs and prints the command to
upgrade it.
