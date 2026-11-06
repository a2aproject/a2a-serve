---
name: user-simulation
description: Run a user-simulation test round of a2a-serve, where parallel subagents start real agents and act as an owner and other users to find bugs in a2a-serve, the a2aacp bridge and the a2a CLI, and poor UX. Covers setup, the lane catalog, the lane brief, consolidating findings, rechecking earlier issues and filing new ones. Use when the user asks for simulation testing, user-simulation, exploratory or hands-on QA, a bug bash or bug hunt against running agents, or to recheck issues found by an earlier simulation run. Don't use for unit or e2e tests (go test), for recording demos (tools/demo), or for code review.
---

# User-simulation testing

A round starts real agents from a fresh build and has testers act as the
people who use them. Each tester is a subagent that covers one lane from
`lanes.md` and writes its findings to a file. You coordinate: you set up
the run, brief the lanes, merge what they find, check the important
findings, and file issues.

A round with six lanes takes about an hour: 10 minutes of setup, 25
minutes of lanes running in parallel, and 20 minutes to consolidate and file.

## 1. Set up

You need Docker, Go, `gh` signed in, and a model key. Export the key and
build the run from `origin/main`:

```bash
export OPENROUTER_API_KEY=...
tools/sim/setup.sh --smoke
```

`SIM_SECRET` names a different key variable, and `SIM_MODEL` sets the
`--model`. For example, with a Gemini API key:

```bash
SIM_SECRET=GOOGLE_GENERATIVE_AI_API_KEY SIM_MODEL=google/gemini-3-flash-preview tools/sim/setup.sh --smoke
```

The run lives in `/tmp/a2a-sim` (`--root` or `SIM_ROOT` change it).
`run.md` there records the commit, and every finding refers to it.

- Test `origin/main` (the default). A finding on an old branch may already
  be fixed. Use `--ref checkout` only to test your own change.
- Fix a failing smoke test before you start lanes. Lanes that can't reach
  a working agent waste the whole budget.
- The model must work with the key. Check it with `a2a-serve start ... --model`
  in the smoke test, since an unknown model fails every turn.

## 2. Pick the lanes

Read `lanes.md`. For a regular round, run all lanes. After a focused
change, run the lanes that cover it. Then list the open issues from
earlier rounds:

```bash
gh issue list --state open --search '"tools/sim" in:body' --limit 100
```

Add each issue to the focus of its lane as "recheck first". The lane
reruns the issue's repro and reports whether it still happens. Close
issues that no longer reproduce, with the commit from `run.md`.

### Add novelty

Repeating the same scenarios finds fewer bugs each round. Change at least
two of these every round, and record the choices in `run.md`:

- **Chaos.** Run `tools/sim/chaos.sh` in the background during the lanes.
  It kills, pauses and cuts the network of agent containers, and freezes,
  restarts and kills servers, at random intervals. It logs each fault to
  `chaos.log`, with the seed, so a round can be replayed with `--seed`.
  Faults are scoped to the run, so it never touches other agents.
- **Personas.** Give each lane a persona that drives how it uses the
  product, drawn from `lanes.md`, such as a user who cancels and retries
  quickly or a user who scripts everything with `-o json`.
- **A wildcard lane.** One lane gets no catalog section. Instead, it gets
  the commits merged since the last round, or the least-covered area in
  the `rounds.md` log, and invents its own scenarios.
- **Environment.** Rotate the model (`SIM_MODEL`), the agent
  (`SIM_AGENT=claude`) or the sandbox (`--sandbox local`).

## 3. Run the lanes

Start one subagent per lane, all in parallel, in the background. Give each
one the text of `brief.md` with the placeholders filled in:

| Placeholder | Value |
| --- | --- |
| `{LANE}` | The lane name, such as `approvals` |
| `{ROOT}` | The run directory, such as `/tmp/a2a-sim` |
| `{REPO}` | `$ROOT/src`, or the checkout for `--ref checkout` |
| `{A2A_GO}` | `$(go env GOMODCACHE)/github.com/a2aproject/a2a-go/v2@<version>`, with the version from `$ROOT/src/go.mod` |
| `{BUDGET}` | Minutes, 5 less than your subagent time limit (25 for a 30-minute limit) |
| `{FOCUS}` | The lane's section of `lanes.md`, plus the issues to recheck |

Use a capable model for `acp` and `multiuser`, which need the most
judgment. The other lanes do well with a mid-size model at high effort.

With chaos on, start it after the lanes and give it the lane budget:

```bash
tools/sim/chaos.sh --minutes 25 --interval 90:240   # background job
```

Tell the lanes that chaos is on, so they check `chaos.log` before they
report a failure. A fault is expected. The finding is how the product
handles it: a task stuck in `working`, an unclear error, a lost
conversation, or no recovery after the fault ends. Don't run chaos in
the round's first run of a lane whose findings you need to be
reproducible, such as rechecking issues.

While the lanes run, `tools/sim/collect.sh` shows what they have found
so far. If a lane stops without a `findings.md`, start it again with a
smaller focus. It may leave notes and outputs in its lane directory that
the new run can reuse.

## 4. Consolidate

```bash
tools/sim/collect.sh > /tmp/a2a-sim/summary.tsv
```

Then, for the merged list:

1. Merge duplicates across lanes. The same bug often shows up in two
   lanes, such as a denied tool call seen by both `acp` and `approvals`.
2. Drop findings that `review/baseline.md` lists, and findings that
   already have an issue (`gh issue list --search '<keywords>'`).
3. Check every high finding, and every medium finding you plan to file,
   against the code at the cited `file:line`. A finding that the code
   doesn't explain needs a rerun before you file it.
4. Write a report sorted by severity, with links to the lane files for the
   repros, and show it to the user before you file anything.

## 5. File issues

Ask the user before filing. Read `SECURITY.md` first: it asks for
security issues to be reported privately. Ask the user how to handle
findings with a security impact, such as an auth bypass, a sandbox escape
or a denial of service.

- Route each finding to the repo that owns the code:
  - `a2aproject/a2a-serve` for the server and its docs.
  - `a2aproject/a2a-go` for `a2aacp`.
  - `a2aproject/a2a-cli` for the `a2a` CLI.
  Link related issues across repos.
- Give each high or medium bug its own issue. Group low findings into
  issues by theme, with a checklist, such as error messages, CLI output, or
  config and docs.
- Write each issue for someone who wasn't there: what happens, the repro
  commands, the expected result, the location in the code, and a fix idea
  if the lane had one.
- End each body with this line, which step 2 searches for:
  `Found by user simulation (tools/sim) at <commit>.`
- Leave out model keys, tokens, local paths with user names, and machine
  names.

## 6. Clean up

```bash
tools/sim/cleanup.sh
```

It stops every agent of the run and lists containers that still mount the
run directory. Stop `chaos.sh` first if it still runs. Delete the run
directory when you no longer need the lane files.

Then append one line to `tools/sim/rounds.md`: the date, commit, lanes,
novelty choices with the chaos seed, and the number of findings and issues
filed. The wildcard lane of the next round reads it to find areas that
haven't been covered recently.

## Turn findings into tests

A finding with a deterministic repro that doesn't need a real model
belongs in `./e2e`, which runs `fakeacp`. Note this in the issue, so the
fix comes with a test. Findings that need real agent behavior are covered
by the harness contract tests proposed in #148.

## Pitfalls

- Every shell must `source env.sh`. A command run with the real `HOME`
  talks to the user's own agents and credentials.
- The agent must be started from the same `HOME` that calls it as the
  owner. The server writes the owner token to the `HOME` it starts in.
- Lanes share the model key. Many parallel lanes can hit rate limits.
  Lanes back off on HTTP 429, and so should you when you reproduce bugs.
- Containers from a lane that hit its time limit keep running. Run
  `cleanup.sh` after every round, including failed ones.
