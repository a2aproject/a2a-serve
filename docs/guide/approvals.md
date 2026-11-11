# Approvals

Every tool call that changes something waits for approval, unless a rule or an earlier "Always allow" decides it.

The task pauses in `input-required` and shows the tool call with three options: `once`, `always` and `reject`. Answer on the same task:

```sh
a2a send --task-id <task-id> once          # or: always, reject, deny
```

In a chat, the options are buttons:

```
coder  Permission required: kubectl get pods -n prod.
       [Allow once] [Deny]
       [Always allow kubectl get *]
```

To give the agent other instructions, answer `reject` first. When you send an answer more than once, such as a retry, see [Approval Messages](../reference/approval-messages.md) for how to number it. `a2a-serve logs` shows who or which rule decided each tool call.

## Always allow

`always` allows the tool call and the calls like it for the rest of the conversation. The option names what it allows:

| Tool call | "Always allow" covers |
| --- | --- |
| An MCP tool | that tool, such as `grafana.query_prometheus` |
| A shell command | the program and the next word, such as `kubectl get *` for `kubectl get pods -n prod` |
| A call of another kind, such as an edit | that kind, such as `edit` |

Commands with several parts (`;`, `&`, `|`) and programs that run other commands, such as `sudo` or `xargs`, have no `always` option. A new conversation asks again.

## Allowed in all your conversations

When you tap "Always allow" for the same calls in 3 conversations in a row, the chat offers to remember it:

```
coder  You allowed kubectl get * in 3 conversations in a row.
       Allow it in all your conversations?
       [Allow for me] [Not now]
Anna   taps "Allow for me"
coder  ✓ kubectl get * runs without asking in your conversations.
       /allowed shows what you allowed, and removes it.
```

`/allowed` lists what you allowed, with a button to remove each. In a chat without buttons, send `/allowed remove kubectl get *`. Owners see what each pairing allowed with `a2a-serve pair get <name>`. See [limits](../reference/limits.md#approvals) for how many calls a person can allow.

## Rules

`--allow-tools` and `--deny-tools` decide tool calls of every caller without asking. They take three forms of entries:

| Entry | Matches | Examples |
| --- | --- | --- |
| A tool kind | the ACP tool kind of the call | `read`, `edit`, `*` |
| `tool:<server>.<tool>` | a call to a tool of an MCP server | `tool:grafana.*`, `tool:gmail.send_message` |
| `command:<command>` | a shell command | `command:kubectl get *`, `command:git push *` |

The kinds are `read`, `edit`, `delete`, `move`, `search`, `execute`, `think`, `fetch`, `switch_mode` and `other`. `*` matches any text.

```console
$ a2a-serve config set --allow-tools 'tool:grafana.*' --allow-tools 'command:kubectl get *' --deny-tools 'command:git push *'
  allow-tools  + tool:grafana.*, + command:kubectl get *
  deny-tools   + command:git push *
✓ saved .a2a/config.yaml
✓ applied to the running agent: allow-tools, deny-tools
```

A command entry matches from the start of the command, and never matches a command with several parts. `command:git push *` doesn't match `git -C repo push`, so deny the kind `execute` to stop every form of a command.

When several entries match a call, the most specific one decides: a tool or command entry before a kind, and a longer pattern before a shorter one. Between equally specific entries, deny decides.

| Lists | Effect |
| --- | --- |
| `--deny-tools execute --allow-tools 'command:kubectl get *'` | `kubectl get` runs. Other commands are denied. |
| `--allow-tools 'command:git *' --deny-tools 'command:git push *'` | Git commands run. `git push` is denied. |

`a2a-serve status` lists the rules, and `a2a-serve config unset allow-tools '<entry>'` removes one.

## Which decides

1. The rules of `--allow-tools` and `--deny-tools`.
2. What the person who started the task allowed in all their conversations.
3. What that person allowed with "Always allow" in the conversation.
4. Otherwise, the person who started the task answers.

So a new `--deny-tools` entry stops what people allowed for themselves.

Rules and grants see only the calls that the agent asks about. opencode doesn't ask before tools that change nothing, such as reading files. Claude Code asks in its default mode, which runs some read-only shell commands without asking.

## Who answers

The person who started a task answers its approvals, in the place the task started. Owners can answer any approval.

| Task started by | Who answers | Where |
| --- | --- | --- |
| An A2A caller | that caller | in the task |
| A messenger user | that user | the same chat |
| Another agent | the person who asked that agent | relayed by their agent: in their task, or in their chat |
| A webhook node | the node | in the task |
| A schedule with `--chat` | the schedule's `--as` user | that chat |
| A schedule without `--chat` | an owner | the owner chat |
| A [guest](identities.md#guests) | an owner | the owner chat |

- Set the **owner chat** with `--owner-chat <messenger>[:<who>]`. Without it, the first messenger with a linked owner is used. Without one, questions wait in the [inspector](inspector.md).
- In a group chat, only the person who started the task, or an owner, can answer. When several questions wait in one chat, reply to the message of the one you answer.
- While a question waits, other messages in that chat don't reach the agent. Answer the question, or send `/stop`.
- [Members](identities.md#roles) approve their own tasks, so use `--deny-tools` to limit what they can run.
- An approval that nobody answers is rejected. See [limits](../reference/limits.md#approvals) for how long it waits.
