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
- **The sandbox reaches the tailnet as the user's device** (from #86 and #354). Its connections leave through the host's Tailscale node, so this agent and the user's other agents give it the owner role. The private network is a network of the user's trusted devices. The fix belongs in the sandbox, which #17 cuts off from the tailnet and the LAN. A fix in the receiving agent takes the owner role away from the user's machines that run agents and breaks the examples, so #371 was reverted.
- **A process on one of the user's own tailnet devices acts as the owner.** The access model trusts the user's devices. A compromised device is out of scope.
- **Tailnet members get the member role by default, so they run tools and approve their own tool calls.** Setup on a trusted network takes no extra steps. `--private-allow-incoming paired` and `--deny-tools` narrow it.
- **Defaults give trusted callers broad access** (from #355). Pairings and private-network members get what the owner's agent gets unless a rule narrows it. Only guests start narrowed. Ease of setup outweighs least privilege for callers the owner chose.
- **Values passed with `--secret` are readable inside the sandbox.** The docs say so, and #16 tracks keeping model keys on the server.
- **With `--sandbox local`, secrets and storage rules don't confine the agent.** Local mode runs the agent on the host as the user, and the docs say so. At most the output needs to say that a rule doesn't apply there.
- **Many callers with private workspace copies can fill the disk** (from #355). Sizing the shared directory and the disk is the owner's job. The copy cap and its error are enough.

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
- **The guests' home directory and stored IDs keep the name `anonymous`** (from the role rename: `state/home-anonymous`, `auth.GuestID`, `agentkey.Guest`). Tasks, conversations, ACP sessions and existing agent directories store them. Renaming them needs a migration of each and changes no behavior.

### User simulation

These came from the user-simulation round at `9341d2f`, and their locations refer to that commit. They are wording or output polish with no lost work and no wrong instruction, or the behavior is intended.

- **`start` with no key for the model's provider starts, and the first task fails.** This is the design from #174. The start banner, `doctor` and the failed task all name the missing key.
- **`start --quiet` still prints the status block** (`internal/cli/start.go:27`). The flag hides only the image pull and build output, as its help says.
- **An answer to a finished task shows the raw state enum, such as `TASK_STATE_COMPLETED`** (`internal/a2aerr/a2aerr.go:89-97`). The message is still clear enough to act on.
- **The per-caller limit of 8 unfinished tasks gives two different refusal texts** (`internal/a2aerr/a2aerr.go:188-189`, `internal/agentsrv/server.go:213`). Both tell the caller to wait.
- **`config set --restart` prints "restart to apply" before it restarts** (`internal/cli/config.go:90`). It's a cosmetic message order.
- **`config unset --description` gets only the generic "unknown flag" hint.** `unset` takes setting names, and the help shows them.
- **`doctor` has no `--timeout` and waits 60 s on a frozen server** (`internal/doctor/agent.go:414`). A frozen server is rare, and the default timeout bounds the wait.
- **`schedule run` points to `logs`, which doesn't name the schedule** (`internal/cli/schedule_run.go:41`), **and a long invalid schedule name is echoed in full without the 64-character limit** (`internal/schedule/schedule.go:38-44`).
- **The `status` hint to turn on a recipe's optional MCP server doesn't restore the mount it needs** (`internal/cli/status.go:269`). It rests on one run that never started the real server.
- **Inspector approval cards don't say who started the task** (`internal/agentsrv/inspector/pages.go:69`). The conversation page shows it.
- **The question for an edit or a fetch doesn't name the action.** #167 tracks the question format.
- **`recipe --yes` reports one missing variable per run** (`internal/cli/recipe.go:437-445`). Each run names the variable and the flag that sets it.
- **`pair invite --webhook -o json` prints a zero `created` time for the new node** (`internal/pairings/pairings.go:240-252`). `pair get` shows the real time.
- **In an agent-to-agent call, the caller's agent may run `a2a send --task-id <callee task> once` itself.** That is model behavior. The callee's real approval reaches the user as its own task.
