# Lanes

Each lane is one area that one tester covers in a run. Pick the lanes that
match what changed since the last run, and give each one its section below
as `{FOCUS}` in `brief.md`. Each lane lists the docs and code to read
first, then the scenarios to try. A lane that needs more than one budget
should be split in two.

## acp

The A2A to ACP bridge, with one user. Read `a2aacp` (`executor.go`,
`turn.go`, `sessions.go`, `convert.go`, `process.go`, `questions.go`) and
`internal/agentsrv/executor.go`, and list the edge cases the code might
mishandle before you run anything.

- Multi-turn memory in one context, before and after `a2a-serve restart`.
  Isolation between contexts.
- Streaming and blocking sends, `--async`, `task get` and `task list`.
- Cancel during a turn, then send again in the same context.
- A second message while a turn runs, or while the turn waits for approval.
- Empty, very long, non-ASCII and emoji messages. File parts of several
  MIME types, including binary and empty files.
- Kill the agent process inside the container during a turn
  (`docker exec ... pkill`). What does the caller see?
- An unknown or failing model (`--model` with a bad ID, a revoked key).
- How tool calls, questions and errors look in the CLI output and task
  history.

## approvals

The approval loop as a caller and as the owner. Read
`docs/guide/approvals.md`, `docs/guide/inspector.md` and
`internal/agentsrv/approvals/`.

- A prompt that edits a file, then a prompt that runs a command. Approve,
  reject, and reply with something that isn't an option.
- Answer a wrong, completed or unknown task ID. Answer twice. Answer
  concurrently from several shells.
- Several approvals in one turn.
- `--allow-tools` and `--deny-tools` at start, and changed live with
  `config set`, including after an earlier `always`.
- Approve from the inspector (`a2a-serve inspect --no-browser`, drive it
  with curl). Stale pages, double posts.
- Leave an approval pending for a long time. Does anything time out?
- Does the question show enough to decide: the command, the file, the diff?

## multiuser

Several people on one agent. Read `docs/advanced/access-model.md`,
`docs/guide/identities.md`, `docs/reference/commands/pair.md` and
`internal/access`. Use at least an owner and three users.

- `pair invite` and `pair join`: valid, reused, expired and garbled codes,
  and two people racing for one code. Revoke a pairing during a task and
  during an approval.
- `--private-access` and `--public-access` values, and what an unpaired
  caller sees.
- Can one user get, list, cancel, continue or approve another user's task
  or conversation? Can the owner join a user's conversation, and can the
  user continue after that?
- Ask each user's agent to remember a different word at the same time,
  then ask each for it.
- Per-user process pool: more users than the pool cap, some with pending
  approvals.
- Tokens in logs, task output and error messages.

## cli

A new user following the docs literally, then the lifecycle commands and
their error paths. Read `docs/getting-started.md`, `README.md` and
`docs/reference/commands/`.

- Run getting started word for word, changing only the model and key.
  Note every place where the docs and the output differ.
- `start` without a terminal, when already running, with a missing key,
  with invalid flag values, and nested inside another agent directory.
- `stop`, `restart`, `status`, `logs`, `doctor` and `doctor --fix` in
  every state, including after a `kill -9` of the server.
- `config show`, `config set`, `config unset` and `config edit` with bad
  values, unknown keys and live versus restart-only settings.
- `schedule`, `backup` and `recipe` happy paths and errors.
- Exit codes on failure (`echo $?`), duplicated output, and hints that
  name the right next command.

## resilience

Failures and recovery. Read `docs/advanced/agent-directory.md`,
`docs/guide/running.md`, `docs/guide/secrets.md` and
`docs/guide/backups.md`.

- `stop` and `restart` during a working task and during an approval, then
  answer the approval afterwards.
- `kill -9` the server during a task, `docker kill` the agent container,
  and a Docker endpoint that doesn't answer.
- A corrupt, empty or deleted `config.yaml`, and a deleted or rotated
  secret.
- Two CLI commands on one agent at once, such as `config set` and `start`.
- `backup` and restore into another directory: do tasks, conversations
  and pairings continue?
- Count the files in `.a2a/state` across ten restarts.

## http

The HTTP surface, with curl. Read `docs/reference/http-routes.md` and
`internal/httpserver`.

- Malformed JSON, wrong content types, unknown methods, oversized bodies,
  thousands of parts, deep nesting.
- Missing, invalid and wrong-scheme tokens on every route.
- Slow headers and slow bodies, with and without auth, from many
  connections. Can one caller lock out the owner?
- CORS on the agent card and on the other routes.
- Response sizes of `ListTasks` and `GetTask` with large histories.

## concurrency

Load and agent-to-agent calls. Read `docs/guide/agent2agent.md` and
`examples/01-agent-server/README.md`.

- Eight concurrent sends in separate contexts, and three into one
  context. Every task reaches the right final state with its own answer.
- Concurrent `--async` sends with cancels.
- One agent calls another through a contact. Approvals of the nested call.
  Stop the callee during the call.
- Mailbox wakes and webhook nodes, if they can run locally.

## wildcard

No fixed scenarios. Give the lane the output of
`git log --oneline <last round's commit>..origin/main` and the last few
lines of `rounds.md`. It picks the changed or least-covered area, reads
its docs and code, and invents the scenarios a user of that feature would
try. It reports which area it chose and why, so the next round can pick
another.

## Personas

Assign one persona per lane, and change them between rounds. A persona
changes how the lane uses the product, not what it tests.

| Persona | Behavior |
| --- | --- |
| Newcomer | Follows the docs word for word and never reads the source. Copies commands from output and hints. |
| Impatient | Cancels, retries and sends follow-ups before a turn ends. Presses Ctrl-C during long commands. |
| Scripter | Uses `-o json` and `jq` for everything, checks exit codes, and runs commands in loops. |
| Careless | Pastes the wrong task ID, types options with typos, runs commands from the wrong directory. |
| Team lead | Invites several people, shares one agent, and revokes access when someone leaves. |
| Adversary | A paired user who tries to read, steer or block other users' work, within the access they were given. |
| Long session | Keeps one conversation going for many turns across restarts, and comes back to old tasks. |

## Not covered yet

These need accounts or infrastructure that a local run doesn't have. Run
them by hand, or give a lane the credentials explicitly:

- Public URLs and relays (`--public`, `a2a-serve relay`, Agent Mesh).
- Tailscale and its private network.
- Messengers: Telegram, Discord and email.
- Claude Code as the agent (`SIM_AGENT=claude` with an Anthropic key).
