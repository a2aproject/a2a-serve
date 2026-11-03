# Upgrades

Upgrade `a2a-serve` like any binary, then restart each agent with
`a2a serve restart` (or `a2a serve` in its directory). An agent picks up the
new version on its next start.

## Stored state migrates forward

- **`server.db`** carries its schema version in SQLite's `PRAGMA
  user_version`. On start, the server applies every migration from that
  version to the current one, each in its own transaction, so a failed
  migration leaves the database as it was. Released migrations never change;
  new versions add new ones.
- **`config.yaml`** written by an earlier version is read by later ones.
  Settings that were renamed or moved are read from their old place and
  written back in the new one the next time a command saves the config, such
  as configs in which the first of `mounts` was the workspace.
- **Renamed commands, routes and variables** keep working for at least one
  release, hidden from help, or fail with a message that names the new
  command. The webhook node commands under `a2a serve relay`, the `/relay/…`
  routes and `A2A_RELAY_TOKEN` are the current example.

## Going back isn't supported

A database migrated by a newer version can't be used by an older one: the
older server refuses to start with "the database is at version N, newer than
this a2a supports". To be able to go back, take a snapshot before upgrading,
and restore it into the agent directory after downgrading:

```sh
a2a serve backup now                 # with --backup set, or copy .a2a/ while the agent is stopped
```

## Harness images

The harness image tag names the `a2a` CLI version the server needs
(`ghcr.io/a2aproject/harness-<harness>:a2a-<version>`), so a new `a2a-serve`
that needs a newer `a2a` pulls a new image on its next start. The harness
home in `state/home` is kept, with its sessions and config.

## The a2a CLI

Callers and nodes use the `a2a` CLI. `a2a serve doctor` checks that the
installed version has what this `a2a-serve` needs and prints the command to
upgrade it.
