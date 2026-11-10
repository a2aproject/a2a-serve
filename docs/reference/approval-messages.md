# Approval Messages

This page describes the A2A messages in which a task asks a question, such as an approval of a tool call, and the replies that answer it. [Approvals](../guide/approvals.md) covers who answers and where.

## Questions

A task that waits for an answer is in `input-required`. Its status message has these parts:

| Part | Content |
| --- | --- |
| Text | The question, and the replies it takes. |
| Data, for a permission | `{"toolCall": {"id", "title", "kind", "input"}}`: the tool call it asks about. |
| Data, tagged as a [Structured Input](https://github.com/a2aproject/experimental-ext-structured-input) schema | The JSON Schema of the answer, with the ID of the question in the part metadata. |

The schema has these properties:

| Property | Content |
| --- | --- |
| `option` | The options, as `oneOf` constants with titles. |
| `content` | The fields of a form. Only forms have it. |
| `seq` | A constant: the number of the question in its task. |

For example, the schema of the second question of a task:

```json
{
  "type": "object",
  "title": "git push",
  "properties": {
    "option": {
      "type": "string",
      "oneOf": [
        {"const": "once", "title": "Allow once"},
        {"const": "always", "title": "Always allow git push *"},
        {"const": "reject", "title": "Deny"}
      ]
    },
    "seq": {"type": "integer", "const": 2, "description": "The number of this question in the task. ..."}
  },
  "required": ["option"]
}
```

A task numbers its questions 1, 2, 3 and so on, in the order it asks them. This includes the questions of other agents that it asks on behalf of their tasks. A question asked again, after a reply that the task couldn't use, keeps its number. The task metadata key `a2a-serve/questions` holds the count of the questions the task asked. The number is stored with the status that asks the question, so it survives a restart of the server. Questions asked by an a2a-serve version before numbers have no `seq`.

## Answers

A reply answers the question in one of these forms:

| Form | Example |
| --- | --- |
| Text with an option or a word for it | `once`, `approve`, `deny` |
| A data part with an `option` field | `{"option": "once", "seq": 2}` |
| A Structured Input response, tagged with the ID of the question | `{"option": "once", "seq": 2}` |

`seq` is optional. When the reply has it, the server checks it against the number of the question that the task asks:

| `seq` of the reply | Result |
| --- | --- |
| The number of the question | The answer applies. |
| A lower number | Refused: that question was already answered. |
| A higher number | Refused: the task hasn't asked that question. |
| Not a positive integer | Refused. |
| Missing | The answer applies to the question that the task asks now. |

A refused reply leaves the question open. The task stays in `input-required` and asks the question again, after a note such as:

```
I couldn't use the reply: it answers question 1 of this task, which was already answered. This is question 2, and it still waits for an answer. Reply with one of: once, always, reject.
```

A Structured Input response for another question ID is refused the same way. A reply to a task that has finished fails with an error that names the task's state.

Send `seq` whenever an answer may arrive twice, as with a retry after a timeout:

```bash
a2a send --task-id <task-id> --data-part '{"option":"once","seq":2}'
```

Without `seq`, a repeated answer answers the next question of the task, if the task asked one meanwhile. Chat buttons, email links and the [inspector](../guide/inspector.md) answer only the question they show, so they need no `seq` from the person who answers.

Planned: the `Resume:` line of `a2a send` shows the reply with the number of the question.
