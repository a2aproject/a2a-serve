# Approvals

Every tool call that changes something waits for approval. The task pauses in `input-required` and shows the tool call with three options: `once`, `always` and `reject`. Answer on the same task:

```sh
a2a send --task-id <task-id> approve      # or: once, always, reject, deny
```

Clients can also answer with the [Structured Input extension](https://github.com/a2aproject/experimental-ext-structured-input), which the agent card advertises.

`--allow-tools` and `--deny-tools` answer by tool kind without asking. The kinds are `read`, `edit`, `delete`, `move`, `search`, `execute`, `think`, `fetch`, `switch_mode` and `other`; `*` matches all of them.

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

- Set the **owner chat** with `--owner-chat <channel>[:<who>]`. Without it, a2a-serve picks the first messenger with a linked owner. Without one, questions wait in the [inspector](inspector.md).
- In a group chat, only the person who started the task, or an owner, can answer.
- A typed answer such as `approve` answers the question whose message it replies to. When several questions wait in one chat, reply to the message of the one you answer. A typed answer that replies to none of them gets a prompt to do so.
- Authenticated callers approve their own tasks, so use `--deny-tools` to limit what they can run.
- Email approvals link to a web page where the person who started the task answers. Approvals that only an owner can answer have no link.
