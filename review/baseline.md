# Review baseline

This page lists findings that past reviews considered and chose not to act on. Don't report them again unless the code changed in a way that makes them reachable or worse.

## Dropped findings

Each entry gives the finding and the reason it was dropped. Locations refer to commit `cb1c826`, except in entries marked "at `5bd409f`".

### Security

- **The inspector session cookie works across loopback ports** (`internal/agentsrv/inspector/inspector.go:145-171`). Exploiting it needs a hostile co-tenant on 127.0.0.1 who also lures the owner to a page.
- **The sandbox on Docker Desktop reaches `localsrv exec` on the host** (`internal/harness/runner.go:547`, `internal/localsrv/serve_exec.go:73-80`). It needs the developer test server running at the same time.
- **Wakes can be replayed within the 5-minute signature skew** (`internal/mailboxapi/signature.go:62-82`). Receivers can dedup on `envelope.id`. At most this needs a note in the docs.
- **Pairing codes are stored as unsalted SHA-256** (`internal/store/pairings.go`). A code has 40 bits and a 15-minute TTL, so the stored hash can't be brute-forced in practice.
- **`CloseQuestions` doesn't mark records closed.** This matters only for an agent that reuses tool-call IDs, and no supported agent does.
- **The control socket fallback in `$XDG_RUNTIME_DIR` or `/tmp` can be covered by a broad `--mount`** (`internal/agentdir/agentdir.go:152-162`). It needs a very long agent directory path plus an unusual mount.
- **A node can read another node's idempotent result before the ownership check** (`internal/agentsrv/nodes/nodes.go:377-389`). It needs the other node's task ID and idempotency key.
- **A private-network member can take over a gossip entry by copying another agent's card name** (at `5bd409f`: `internal/agentsrv/ads.go:128`, `internal/gossip/gossip.go:480-500`). Private-network members get this much trust in the access model.
- **`relay invite <name>` lets a removed node back in through a name it once published** (at `5bd409f`: `internal/relay/control/control.go:216-247`). Inviting is an explicit action of the relay admin.
- **Backup extraction has no size or entry limits, and restores symlinks at any path** (at `5bd409f`: `internal/backup/snapshot.go:417-485`). Writing a snapshot needs the backup key.
- **`/n/{node}` answers 404 before authentication** (at `5bd409f`: `internal/agentsrv/nodes/nodes.go:274-285`). Node names have little value to an attacker.
- **Pairing-code failure limits apply per client, with no global cap** (at `5bd409f`: `internal/pairings/limiter.go:56-74`). A code has 40 bits and a 15-minute TTL.
- **The email channel trusts `Authentication-Results` with its authserv-id without tying it to the hop that added it** (at `5bd409f`: `internal/messenger/email/authres.go:201-214`). This is safe when the mail server strips foreign headers with its own authserv-id, as mail servers do. At most this needs a note in the docs.
- **Remote wake URLs may use plain http and follow redirects** (from #86). #118 already refuses redirects to private or local addresses and https-to-http downgrades. Requiring https would break working wake targets.
- **Mail doesn't have to name the agent's address in `To` or `Cc`** (from #86). Requiring it drops mail that reaches the agent through `Bcc` or a list, and replaying a signed mail needs a mail the sender addressed to someone else.
- **The owner role works over a linked email without an opt-in** (from #86). Email identity proves only the domain; the owner links the address on purpose. At most this needs a note in the docs.
- **The owner role is granted on the private listener to connections from the server's own machine** (from #86). The sandbox can reach its own tailnet URL. This stays until the sandbox network allowlist (#17) lands.

### Correctness

- **A first request right after a restart can have its new task failed by `FailUnfinished`** (`internal/agentsrv/nodes/nodes.go:141-152`). The window is narrow and the impact is low.
- **The inspector lists at most 100 pending approvals** (`internal/agentsrv/inspector/inspector.go:271`). This is cosmetic at the expected load.
- **A failed backup can delete another backup's snapshot taken in the same second** (at `5bd409f`: `internal/backup/backup.go:99-117`). It needs two backups in the same second and a failure of one.
- **Restore doesn't fsync the restored files before it reports success** (at `5bd409f`: `internal/backup/snapshot.go:485`). It needs a crash right after the restore.
- **Migrations read the schema version outside their transaction** (at `5bd409f`: `internal/store/store.go:519`). The offline CLI opens the database only while the server is stopped.
- **One-time `--at` schedules skip the 5-minute minimum between runs** (at `5bd409f`: `internal/schedulesvc/schedules.go:59-62`). A one-time schedule runs once, and the cap of 50 schedules still holds.

### Performance

- **The event bus subscription queue has no bound** (`internal/agentsrv/executor.go:504-533`). The length of one task bounds it.
- **The `localsrv` test servers keep tasks and output in memory.** They are for development only.
- **SQLite `auto_vacuum` is off.** SQLite reuses freed pages, so the file stops growing after the first prune.

### Architecture and developer experience

- **The `schedule` package imports `pflag`** (`internal/schedule/schedule.go:27,150`). The CLI and the in-sandbox schedules command share the flag definitions on purpose.
- **Some package and type names differ from the names in the architecture doc**, for example `harness` for "Sandbox" and `HarnessProxy` in `agentproxy`. The packages extracted in #80 (`messenger`, `agentproxy`, `schedules`) follow the doc, and renaming the rest is churn without a change in behavior.
- **The secret-value help paragraph repeats on several commands, and durations print in Go format such as `5m0s`.** This is help-text polish.
- **Caller-side `pair` commands could move under their own noun.** This is a redesign preference. The exit-code inconsistency is tracked in #81.
- **`start <agent>` takes only the core settings, and `config set` takes all of them.** The split is deliberate. Operational settings, such as auth keys, backups and resource limits, are set with `config set`.
- **`config set` with no flags exits 0.** It changes nothing, so success is the right result.
- **`backup` takes `--deadline`, and other commands take `--timeout`.** The two flags mean different things. `--timeout` bounds each call to the running agent, and `--deadline` bounds the whole command.
- **`restart`, `config set --restart` and `config unset --restart` all restart the agent.** The flags apply a change and restart in one step, and each form is documented.
- **A2A request bodies have a 30-second idle timeout and no total read deadline.** #115 chose this.
- **`card-skills` can be set only with `config edit`, and `config unset` can clear it.** `docs/reference/configuration.md` documents this.
