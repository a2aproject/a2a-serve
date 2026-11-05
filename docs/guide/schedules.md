# Schedules

A schedule runs a prompt at set times: a weekday briefing, triage every two hours, a reminder on Friday at 5.

```sh
a2a-serve schedule add morning-brief --cron "0 7 * * 1-5" --tz Europe/Zurich \
  --prompt "Summarize today's meetings." --chat telegram:owner
a2a-serve schedule add triage --cron "@every 2h" --prompt "Triage new issues." --thread same
a2a-serve schedule add report --at "2026-11-06 17:00" --prompt "Remind me to send the report." --chat telegram
a2a-serve schedule list
a2a-serve schedule run morning-brief   # run it now
a2a-serve schedule rm morning-brief
```

- `--cron` takes five fields or a descriptor such as `@daily` or `@every 2h`. `--at` runs once and then removes the schedule. `--tz` defaults to the machine's time zone.
- `--as` runs the schedule as a pairing; the default is the owner.
- `--chat <channel>[:<who>]` posts each run to that user's chat, where they can reply and answer approvals. `--thread new` (the default) starts a new conversation each run; `--thread same` continues the current one. Without `--chat`, approvals go to the owner chat.
- `--cron` follows the wall clock of the time zone. When clocks fall back, a time in the repeated hour runs once. When clocks spring forward, a time in the skipped hour runs right after it. `@every` counts elapsed time and ignores clock changes.
- A run missed while the agent was stopped runs once on the next start.
- A one-time schedule stays until the agent accepts its run. If the agent stops while the run waits for an answer, the run happens again on the next start.
- A run that comes due while its conversation waits for an answer, such as an approval, waits until the answer arrives. Later runs of the same schedule that come due meanwhile merge into the waiting run, which tells the agent how many runs it skipped. Removing the schedule drops the waiting run.

The agent can create schedules too. When someone asks "remind me on Friday at 5", the agent adds a schedule that runs as that person and reports back in the same conversation. The server posts a notice in the chat when the agent adds a schedule. Each person can have at most 50 schedules from the agent, and their runs must be at least 5 minutes apart. `a2a-serve schedule add` has neither limit. People can list and remove only their own schedules; `a2a-serve schedule list` shows all of them. Each run gets the access its person has at that time, so the schedule of someone on the tailnet fails once they leave it.

Not in this version: schedules in `config.yaml`, and editing a schedule. Remove it and add it again.
