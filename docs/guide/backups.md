# Backups

Backups keep encrypted snapshots of an agent's conversations, pairings, secrets and network identity, so you can restore it on another machine.

## Take backups

```sh
a2a-serve config set --backup gs://my-bucket/agents/coder --backup-key env:BACKUP_KEY --backup-interval 1h
a2a-serve backup now
a2a-serve backup list
```

- `--backup` is a directory, `gs://bucket/prefix` or `s3://bucket/prefix`. Cloud Storage and S3 use the `gcloud` and `aws` CLIs and their usual credentials.
- Give each agent a prefix of its own, such as `gs://my-bucket/agents/<agent>`. A restore takes the latest snapshot in the destination, of any agent.
- Keep a copy of `--backup-key` somewhere other than the backups. A restore needs it.
- A running agent takes a snapshot every `--backup-interval` and removes snapshots older than `--backup-retention`. The destination needs permission to delete objects for that. `a2a-serve status` shows a failure until the next successful backup. On a running agent, `a2a-serve backup now` counts as one, and the next scheduled snapshot comes an interval later. The [command reference](../reference/commands/config.md#a2a-serve-config-set) lists the defaults.
- A snapshot doesn't include the workspace, which is usually in git. It also leaves out the private copies of the workspace that members get with [`--storage own`](sandbox.md#set-what-a-pairing-gets), so a restore loses the work in them that isn't committed and pushed.
- A snapshot includes `.a2a/recipe.lock`, so `a2a-serve recipe` can update a restored agent that came from a [recipe](recipes.md).

## Restore on another machine

```sh
a2a-serve backup restore gs://my-bucket/agents/coder --dir ~/src/app --backup-key env:BACKUP_KEY
```

The restore takes the latest snapshot. `--snapshot` names another one. A destination that several agents share needs `--snapshot`, since the latest snapshot there can belong to another agent. Snapshot names start with the agent name.

The directory must have no `.a2a`, or only the `config.yaml`, `recipe.lock` and `.gitignore` of a fresh clone. The snapshot's `config.yaml` replaces the one in the clone, and the output says so when they differ. If commands such as `a2a-serve pair list` created `.a2a/state` before the restore, and nobody used the agent, the restore moves it to `.a2a/state.before-restore-<time>` and says so.

A failed or interrupted restore leaves the directory as it was, so you can run it again. If the restore is killed, the next restore, `a2a-serve start` or `a2a-serve doctor --fix` removes what it left.

Add `--take-over` to keep the agent's network identity and URLs. Stop the agent on the old machine first.

The agent's sessions belong to the workspace path. After a restore into another directory, a message to an earlier conversation starts a new session. The agent gets the earlier messages of the conversation, and the reply says so.

Continuous replication is not available yet, so changes since the last snapshot are lost with the machine.
