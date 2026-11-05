# Review baseline

This page lists findings that past reviews considered and chose not to act on. Don't report them again unless the code changed in a way that makes them reachable or worse.

## Dropped findings

Each entry gives the finding and the reason it was dropped. Locations refer to commit `cb1c826`.

### Security

- **The inspector session cookie works across loopback ports** (`internal/agentsrv/inspector/inspector.go:145-171`). Exploiting it needs a hostile co-tenant on 127.0.0.1 who also lures the owner to a page.
- **The sandbox on Docker Desktop reaches `localsrv exec` on the host** (`internal/harness/runner.go:547`, `internal/localsrv/serve_exec.go:73-80`). It needs the developer test server running at the same time.
- **Wakes can be replayed within the 5-minute signature skew** (`internal/mailboxapi/signature.go:62-82`). Receivers can dedup on `envelope.id`. At most this needs a note in the docs.
- **Pairing codes are stored as unsalted SHA-256** (`internal/store/pairings.go`). A code has 40 bits and a 15-minute TTL, so the stored hash can't be brute-forced in practice.
- **`CloseQuestions` doesn't mark records closed.** This matters only for an agent that reuses tool-call IDs, and no supported agent does.
- **The control socket fallback in `$XDG_RUNTIME_DIR` or `/tmp` can be covered by a broad `--mount`** (`internal/agentdir/agentdir.go:152-162`). It needs a very long agent directory path plus an unusual mount.
- **A node can read another node's idempotent result before the ownership check** (`internal/agentsrv/nodes/nodes.go:377-389`). It needs the other node's task ID and idempotency key.

### Correctness

- **A first request right after a restart can have its new task failed by `FailUnfinished`** (`internal/agentsrv/nodes/nodes.go:141-152`). The window is narrow and the impact is low.
- **The inspector lists at most 100 pending approvals** (`internal/agentsrv/inspector/inspector.go:271`). This is cosmetic at the expected load.

### Performance

- **The event bus subscription queue has no bound** (`internal/agentsrv/executor.go:504-533`). The length of one task bounds it.
- **The `localsrv` test servers keep tasks and output in memory.** They are for development only.
- **`guardedHandler.ListTasks` runs one primary-key lookup per listed task** (`internal/agentsrv/harness_proxy.go:292-309`). There are at most 100 per page, so the cost is negligible.
- **SQLite `auto_vacuum` is off.** SQLite reuses freed pages, so the file stops growing after the first prune.

### Architecture and developer experience

- **The `schedule` package imports `pflag`** (`internal/schedule/schedule.go:27,150`). The CLI and the in-sandbox schedules command share the flag definitions on purpose.
- **Package and type names differ from the names in the architecture doc**, for example `harness` for "Sandbox". Rename them when the packages are extracted (#80).
- **The secret-value help paragraph repeats on several commands, and durations print in Go format such as `5m0s`.** This is help-text polish.
- **Caller-side `pair` commands could move under their own noun.** This is a redesign preference. The exit-code inconsistency is tracked in #81.
