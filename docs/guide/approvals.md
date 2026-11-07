# Approvals

Every tool call that changes something waits for approval, unless a rule or an earlier "Always allow" decides it. The task pauses in `input-required` and shows the tool call with three options: `once`, `always` and `reject`. Answer on the same task:

```sh
a2a send --task-id <task-id> approve      # or: once, always, reject, deny
```

In a chat, the options are buttons:

```
coder  Permission required: kubectl get pods -n prod.
       [Allow once] [Deny]
       [Always allow kubectl get *]
```

A reply that chooses none of the options is refused with the list of options, and the question keeps waiting. To give the agent other instructions, answer `reject` first.

While a task waits for an approval, a new message to its conversation is refused with the task ID and the question. Answer the question first, or leave out the context ID to start a new conversation. Messages of other people in a group chat, and runs of schedules, wait until the question is answered.

Clients can also answer with the [Structured Input extension](https://github.com/a2aproject/experimental-ext-structured-input), which the agent card advertises. A data part with an `option` field answers too, such as `a2a send --task-id <task> --data-part '{"option":"once"}'`.

When a tool call is rejected or denied, the status message of the finished task says so, such as `Tool call rejected: git push.` or `Tool call denied by --deny-tools: git push.` `a2a-serve logs` has a line for each answer: who gave it, or which rule, preference or grant, and the option. The line leaves out the command and the tool input, which can hold secrets. For a preference or grant, the line shows only the program of the command, such as `command:printf ...`.

## Always allow

`always` allows the tool call and the calls like it for the rest of the conversation. The option names what it allows:

| Tool call | "Always allow" covers |
| --- | --- |
| An MCP tool | that tool, such as `grafana.query_prometheus` |
| A shell command | the program and the next word, such as `kubectl get *` for `kubectl get pods -n prod` |
| A call of another kind, such as an edit | that kind, such as `edit` |

The next word counts only when it holds letters, digits, `.`, `_` and `-`, so `ls -R` gives `ls *`. Replies typed without buttons show what `always` covers, such as `always (allows kubectl get * in this conversation)`.

Some questions have no `always` option:

- A command with `;`, `&`, `|`, `` ` ``, `$(`, `<`, `>` or a line break, which has several parts.
- A command that starts with a path or an assignment.
- A command of a program that runs other commands, such as `sudo`, `env`, `xargs` or `find`.
- A shell, interpreter or tool such as `bash`, `python3`, `git` or `kubectl` followed by a flag, as in `bash -c` or `git -C repo push`.
- A call of the kinds `execute` and `other` that isn't an MCP tool.

The grant covers the tasks of the person whose question it answered. A new conversation asks again. The server keeps the grant and answers the agent `once` for each call it covers, so the agent's own "always" never applies. Only an agent that offers no way to allow a call once gets its own "always" answer.

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

Only your answers to the questions of your own tasks count. Denying such a call, or "Not now", starts the count over. The offer appears in a chat with buttons. Only you can answer it, and messages you type after it go to the agent as usual.

`/allowed` lists what you allowed, with a button to remove each. In a chat without buttons, send `/allowed remove kubectl get *`. Owners see what each pairing allowed with `a2a-serve pair get <name>`, and `a2a-serve pair rm` deletes it with the pairing. A person allows at most 50 calls this way.

## Rules

`--allow-tools` and `--deny-tools` decide tool calls of every caller without asking. They take three forms of entries:

| Entry | Matches | Examples |
| --- | --- | --- |
| A tool kind | the ACP tool kind of the call | `read`, `edit`, `*` |
| `tool:<server>.<tool>` | a call to a tool of an MCP server | `tool:grafana.*`, `tool:gmail.send_message` |
| `command:<command>` | a shell command | `command:kubectl get *`, `command:git push *` |

- The kinds are `read`, `edit`, `delete`, `move`, `search`, `execute`, `think`, `fetch`, `switch_mode` and `other`.
- `*` matches any text. A trailing ` *` also matches the bare command, so `command:git status *` matches `git status`.
- A command entry never matches a command with several parts. Such a command asks, unless a kind entry decides it.
- A command entry matches from the start of the command, with runs of spaces counted as one. `command:git push *` doesn't match `git -C repo push`, so deny the kind `execute` to stop every form of a command.
- MCP tools are named by the MCP servers of the project's `.mcp.json`. A tool of a server that opencode set up itself matches by the name opencode gives it, such as `tool:github_create_issue`.

```console
$ a2a-serve config set --allow-tools 'tool:grafana.*' --allow-tools 'command:kubectl get *' --deny-tools 'command:git push *'
  allow-tools  + tool:grafana.*, + command:kubectl get *
  deny-tools   + command:git push *
✓ saved .a2a/config.yaml
✓ applied to the running agent: allow-tools, deny-tools
```

When several entries match a call, the most specific one decides: a tool or command entry before a kind, and a longer pattern before a shorter one. Between equally specific entries, deny decides.

| Lists | Effect |
| --- | --- |
| `--deny-tools execute --allow-tools 'command:kubectl get *'` | `kubectl get` runs. Other commands are denied. |
| `--allow-tools 'command:git *' --deny-tools 'command:git push *'` | Git commands run. `git push` is denied. |
| `--deny-tools '*' --allow-tools read` | Reads run. Everything else is denied. |

An entry is in one list at most: setting it in one list removes it from the other. `a2a-serve status` lists the rules, and `a2a-serve config unset allow-tools '<entry>'` removes one.

## Which decides

1. The rules of `--allow-tools` and `--deny-tools`.
2. What the person who started the task allowed in all their conversations.
3. What that person allowed with "Always allow" in the conversation.
4. Otherwise, the person who started the task answers.

So a new `--deny-tools` entry stops what people allowed for themselves. A change of the rules, a call allowed in all conversations, or an "Always allow" also answers the approvals that wait and that it decides.

Rules, preferences and grants see only the calls that the agent asks about. opencode asks before every tool except the ones that change nothing, such as reading and searching files. Claude Code asks in its default mode.

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
| An [anonymous caller](identities.md#anonymous-callers) | an owner, within 10 minutes | the owner chat |

- Set the **owner chat** with `--owner-chat <messenger>[:<who>]`, on a messenger the agent has. Without it, a2a-serve picks the first messenger with a linked owner. Without one, questions wait in the [inspector](inspector.md).
- In a group chat, only the person who started the task, or an owner, can answer.
- A message that isn't an answer doesn't reach the agent while a question waits for you in that chat. Answer the question first, or send `/stop`.
- A typed answer such as `approve` answers the question whose message it replies to. When several questions wait in one chat, reply to the message of the one you answer. A typed answer that replies to none of them gets a prompt to do so.
- Authenticated callers approve their own tasks, so use `--deny-tools` to limit what they can run.
- An approval in a conversation of an authenticated caller is rejected after 4 hours without an answer. Until then, it keeps the caller's agent process running.
- Other approvals, such as those of owners' tasks and schedules, are rejected after 24 hours without an answer. A message that waits behind an approval waits as long.
- Email approvals link to a web page where the person who started the task answers. Approvals that only an owner can answer have no link.
