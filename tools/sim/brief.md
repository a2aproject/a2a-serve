# Lane brief

The coordinator sends this brief to each lane, with `{LANE}`, `{ROOT}`,
`{REPO}`, `{A2A_GO}`, `{BUDGET}` and `{FOCUS}` filled in. `{FOCUS}` is the
lane's section of `lanes.md` plus any open issues it should recheck.

---

You are testing a2a-serve by acting as its users. a2a-serve runs a coding
agent behind the A2A protocol. Your goal is to find bugs in a2a-serve, in
the `a2aacp` bridge it uses, and in the a2a CLI, plus poor UX, by running
real commands against real agents.

**Code (read-only).** a2a-serve is at `{REPO}`, at the commit in
`{ROOT}/run.md`. The `a2aacp` bridge is at `{A2A_GO}/integrations/executor/a2aacp`.
Don't edit them and don't run git commands that write. Read
`{REPO}/review/baseline.md` and don't report what it lists. The docs in
`{REPO}/docs` and `{REPO}/README.md` say how things should behave. When
reality differs from the docs, report it.

**Environment.** Start every shell with `source {ROOT}/env.sh {LANE} <user>`.
`<user>` is the simulated person, such as `owner`, `anna` or `bob`. Each
person gets their own `HOME` under `{ROOT}/lanes/{LANE}/users/`. Never run
`a2a-serve` or `a2a` with the real `HOME`. The env file also provides:

- `sim_start [flags]`: starts the agent of the current directory with the
  run's model and key, then runs `a2a-serve doctor --fix`.
- `sim_url`: the local URL of the agent of the current directory.
- `{REPO}/tools/sim/newproj.sh <dir>`: creates a small git project.

For example:

```bash
source {ROOT}/env.sh {LANE} owner
{REPO}/tools/sim/newproj.sh "$SIM_LANE/proj" && cd "$SIM_LANE/proj"
sim_start
a2a send -a "$(sim_url)" "What does calc.py do?"
```

**Rules.**

- Work only under `{ROOT}/lanes/{LANE}`. Let ports default.
- Don't use external services such as Tailscale, public URLs or
  messengers unless your focus says so.
- Don't touch containers or agents you didn't start.
- Never print the model key or tokens, and keep them out of your findings.
- Keep model prompts short. On HTTP 429, wait and retry, and note it.
- Confirm every finding by running it. Read the code to explain the cause
  and give a `file:line`. Run any behavior that looks flaky several times.
  Separate real bugs from your own misuse.

**Chaos.** When `{ROOT}/chaos.log` exists, a chaos script injects faults
into your agents during the round: killed or paused containers, cut
networks, and frozen, restarted or killed servers. Before you report a
failure, check the log for a fault at that time. A fault itself isn't a
finding. Report how the product handled it: a task stuck in `working`, an
unclear error, lost state, or no recovery after the fault ended. Include
the `chaos.log` line in the finding.

**Time budget: {BUDGET} minutes.** Create `{ROOT}/lanes/{LANE}/findings.md`
at the start, and append each finding as soon as you confirm it. Stop
testing 5 minutes before the budget runs out. A lane that runs out of time
before writing anything produces nothing.

**Findings format.** `tools/sim/collect.sh` parses it:

```markdown
## 1. <short title stating the problem>
- Category: bug-a2a-serve | bug-a2aacp | bug-a2a-cli | ux | docs
- Severity: high | medium | low
- Repro: exact commands
- Expected: what the docs or a reasonable user expect
- Actual: what happened, with short output excerpts
- Location: file:line, if known
- Confidence: high | medium | low, and how many times it reproduced
```

End the file with a `## Worked fine` section listing the scenarios that
passed, and a `## Not tested` section.

**Cleanup.** At the end, run `a2a-serve stop` in each agent directory you
used, and check `docker ps` for leftover containers. Reply with the path of
`findings.md` and one line per finding, sorted by severity.

**Your focus:**

{FOCUS}
