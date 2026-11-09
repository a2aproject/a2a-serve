# Backups

Backups keep encrypted snapshots of an agent's conversations, pairings, secrets and network identity, so you can restore it on another machine.

```sh
a2a-serve config set --backup gs://my-bucket/agents/coder --backup-key env:BACKUP_KEY --backup-interval 1h
a2a-serve backup now
a2a-serve backup list
a2a-serve backup restore gs://my-bucket/agents/coder --dir ~/src/app --backup-key env:BACKUP_KEY
```

- `--backup` is a directory, `gs://bucket/prefix` or `s3://bucket/prefix`. Cloud Storage and S3 use the `gcloud` and `aws` CLIs and their usual credentials.
- Keep a copy of `--backup-key` somewhere other than the backups. A restore needs it.
- A running agent takes a snapshot every `--backup-interval` (default 24h) and removes snapshots older than `--backup-retention` (default 7d). The latest one is always kept.
- Snapshots don't include the workspace, which is usually in git. They include `.a2a/recipe.lock`, so `a2a-serve recipe` can update a restored agent that came from a [recipe](recipes.md).
- The agent keeps running during a snapshot. A file that changes while it is copied is copied again, up to three times. If it still changes, the snapshot keeps the last copy and the server log names the file.
- `backup restore` restores the latest snapshot, or the one named with `--snapshot`. Add `--take-over` to keep the agent's network identity and URLs; stop the agent on the old machine first.
- `backup restore` needs a directory without a `.a2a` directory, or with one that holds only `recipe.lock` or `.gitignore`, as a clone of a repository that commits only those has. The restore keeps those files, since they match the workspace, and says so when the snapshot's `recipe.lock` differs. It extracts the snapshot next to `.a2a` and moves it into place at the end. A damaged snapshot or Ctrl-C leaves the directory as it was, so you can run the restore again. The next restore removes what a killed restore left next to `.a2a`.
- The agent's sessions belong to the workspace path. After a restore into another directory, or after the agent's directory moves, a message to an earlier conversation starts a new session. The agent gets the earlier messages of the conversation, and the reply notes the new session.

Not in this version: continuous replication. Changes since the last snapshot are lost with the machine.
