# a2a-serve backup

<!-- Generated from the command definitions; do not edit. -->

Snapshots hold a consistent copy of the database (conversations, pairings,
contacts, schedules), config.yaml, the secrets, recipe.lock, the agent home
directory and the network identity, encrypted with --backup-key. A running agent takes one every
--backup-interval and keeps them for --backup-retention, always keeping the
latest. The workspace isn't included: it is usually in git.

Set them up with:

```
a2a-serve config set --backup gs://my-bucket/agents/coder --backup-key env:BACKUP_KEY --backup-interval 1h
```

Recreate the agent on another host with "a2a-serve backup restore".

```
a2a-serve backup <command>
```

**Flags of every `a2a-serve backup` command**

| Flag | Default | Description |
|---|---|---|
| `--deadline duration` | `10m0s` | Time limit of the command, which writes to or lists the backup destination, a duration such as 30s or 15m |
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |

## a2a-serve backup list

List the agent's snapshots, oldest first

```
a2a-serve backup list
```

Aliases: `ls`

## a2a-serve backup now

Take a snapshot now

```
a2a-serve backup now
```

## a2a-serve backup restore

Recreate an agent's .a2a directory from a snapshot taken by
"a2a-serve backup now" or the running agent: its database, config, secrets,
recipe.lock and agent home directory. The latest snapshot is used unless
--snapshot names one.

The directory must have no .a2a, or a .a2a that holds only recipe.lock or
.gitignore, such as those a repository commits. Restore keeps those files.

The network identity (the tailnet machine or the Agent Mesh node) is restored
only with --take-over, so the old and the new host don't run as the same
machine. Without it, the agent joins the network as a new machine on its
first start. Stop the agent
on the old host before taking over.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve backup restore <backup-destination> [flags]
```

**Examples**

```bash
a2a-serve backup restore gs://my-bucket/agents/coder --dir ~/src/app --backup-key env:BACKUP_KEY
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--backup-key string` |  | The backup key (secret value: env:VAR, -, or the value) |
| `--dir string` |  | Directory of the restored agent (default: the current directory) |
| `--snapshot string` |  | Snapshot to restore (default: the latest) |
| `--take-over` |  | Also restore the network identity; stop the agent on the old host first |
