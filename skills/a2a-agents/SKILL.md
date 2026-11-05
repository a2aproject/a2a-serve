## Calling other agents

You run inside an agent server, and you can delegate work to other agents
over A2A with the `a2a` CLI. The first message of a conversation, and now
and then a later one, starts with an `[a2a agents]` line with the agents URL
of this conversation, such as `http://a2a/k3`, and the agents you can call.
Keep using that URL: it stays the same for the whole conversation. It works
only while you answer a message, and the server makes the call for the user
of the conversation.

    a2a send -a http://a2a/k3/<name> "Review the open PR in this repo"

`send` waits for the answer and prints it. If the work may take more than a
minute, add `--async`: it prints the task ID at once, and the result is
posted into this conversation as a new message when it finishes. Tell the
user and end your turn instead of waiting. The server stops watching a task
that keeps its state for 24 hours, or after 7 days, and tells you so.

    a2a send -a http://a2a/k3/<name> --async "Run the full test suite"
    a2a task get -a http://a2a/k3/<name> <task-id>

Continue a task or a conversation with the other agent:

    a2a send -a http://a2a/k3/<name> --task-id <task-id> "Use the main branch"
    a2a send -a http://a2a/k3/<name> --context-id <context-id> "And now the docs"

- When the other agent asks a question (input-required), answer it with
  `--task-id` if you know the answer, or ask the user.
- When the other agent needs an approval for a tool call, the user is asked in
  their chat. You can't answer approvals: end your turn, and the result is
  posted into this conversation once the user decides.
- Only the agents in the `[a2a agents]` line are reachable. The owner adds
  more with `a2a-serve contact add`.
- Only the tasks and contexts this conversation started are reachable.

## Schedules

The built-in `schedules` agent runs a prompt later in this conversation, as
the user you are talking to: a reminder, or a recurring check. Each run sends
you the prompt as a new message, and your answer reaches the user in their
chat. Send one command per message:

    a2a send -a http://a2a/k3/schedules 'add report-reminder --at "2026-11-06 17:00" --tz Europe/Zurich --prompt "Remind the user to send the weekly report."'
    a2a send -a http://a2a/k3/schedules 'add standup --cron "0 9 * * 1-5" --tz Europe/Zurich --prompt "List the PRs waiting for review."'
    a2a send -a http://a2a/k3/schedules list
    a2a send -a http://a2a/k3/schedules 'rm standup'

- `--at` runs once; `--cron` takes five fields or `@daily`, `@every 2h`.
- A user can have at most 50 schedules, with runs at least 5 minutes apart.
- Ask the user for their time zone if you don't know it.
- Write the prompt for yourself at the time of the run, with everything you
  will need then.
