# Schedules

A schedule runs a prompt at set times: a weekday briefing, triage every two hours, a reminder on Friday at 5.

```sh
a2a-serve schedule add morning-brief --cron "0 7 * * 1-5" --tz Europe/Zurich \
  --prompt "Summarize today's meetings." --chat telegram:owner
a2a-serve schedule add triage --cron "@every 2h" --prompt "Triage new issues." --thread same
a2a-serve schedule add report --at "2030-11-06 17:00" --prompt "Remind me to send the report." --chat telegram
a2a-serve schedule list
a2a-serve schedule run morning-brief   # run it now
a2a-serve schedule rm morning-brief
```

- `--cron` takes five fields or a descriptor such as `@daily` or `@every 2h`. `--at` runs once and then removes the schedule. `--tz` defaults to the machine's time zone.
- `--as` runs the schedule as a pairing. The default is the owner. Each run gets the access its person has at that time, so the schedule of someone on the tailnet fails once they leave it.
- `--chat <messenger>[:<who>]` posts each run to that user's chat, where they can reply and answer approvals. `--thread new` (the default) starts a new conversation each run. `--thread same` continues the current one. Without `--chat`, approvals go to the owner chat.
- A run missed while the agent was stopped runs once on the next start.
- When the agent is busy, the server retries a run until it is accepted. A run that comes due while its conversation waits for an answer waits for it, and later runs of the schedule merge into it.

The agent can create schedules too. When someone asks "remind me on Friday at 5", the agent adds a schedule that runs as that person and reports back in the same conversation. The server posts a notice in the chat when the agent adds a schedule. See [limits](../reference/limits.md#schedules) for how many schedules a person can have.

People can list and remove only their own schedules. `a2a-serve schedule list` shows all of them. In a group chat, the agent manages schedules as the person who started the conversation, and runs continue the conversation like a message from that person.

Not in this version: schedules in `config.yaml`, and editing a schedule. Remove it and add it again.
