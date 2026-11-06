# a2a-serve schedule

<!-- Generated from the command definitions; do not edit. -->

Run a prompt on a schedule with no person on the other side, such as a weekday
briefing. Each run is an ordinary task of the agent, run as a chosen user,
and its result and approval requests can go to a chat, where replying
continues the conversation. A run missed while the agent was stopped runs
once when it starts.

The agent can also create schedules during a conversation, such as for
"remind me on Friday at 5"; they run as the user who asked and report back
in the same conversation.

```
a2a-serve schedule <command>
```

**Flags of every `a2a-serve schedule` command**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |

## a2a-serve schedule add

Add a schedule

```
a2a-serve schedule add <name> [flags]
```

**Examples**

```bash
a2a-serve schedule add morning-brief --cron "0 7 * * 1-5" --tz Europe/Zurich \
  --prompt "Summarize today's meetings." --chat telegram:owner
a2a-serve schedule add triage --cron "@every 2h" --prompt "Triage new issues." --thread same
a2a-serve schedule add report --at "2026-11-06 17:00" --prompt "Remind me to send the report." --chat telegram
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--as string` |  | Run as the owner or a pairing name (default: owner) |
| `--at time` |  | Run once, at a time such as "2026-11-06 17:00" or RFC 3339 |
| `--chat messenger[:who]` |  | Show the runs, and ask their approvals, in a chat: messenger[:who], such as telegram:owner (default: no chat; approvals go to the owner chat) |
| `--cron expression` |  | Run on a cron expression of five fields, such as "0 7 \* \* 1-5", or a descriptor such as @daily or @every 2h |
| `--prompt string` |  | The prompt each run sends to the agent |
| `--thread new\|same` | `new` | Run in a new conversation each time, or continue the same one: new\|same |
| `--tz zone` |  | Time zone of --cron and --at, such as Europe/Zurich (default: the server's) |

## a2a-serve schedule list

List schedules, with their last and next runs

```
a2a-serve schedule list
```

Aliases: `ls`

## a2a-serve schedule rm

Remove a schedule

```
a2a-serve schedule rm <name>
```

Aliases: `delete`

## a2a-serve schedule run

Run a schedule now, without changing its next run

```
a2a-serve schedule run <name>
```
