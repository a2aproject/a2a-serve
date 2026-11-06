---
name: record-demo
description: Record terminal demos of the a2a-serve examples with asciinema and tmux, and browser demos (inspector, Telegram Web) with Playwright. Use when the user asks to record, re-record, or dry-run a demo or screencast of an example in examples/, to add or fix a demo scenario, or to turn a recording into a GIF or video.
---

# Recording demos

`tools/demo/record.sh` plays a scenario from `tools/demo/scenarios/` in tmux
panes and records it with asciinema. It types each command at human speed,
waits for the shell prompt, and reads URLs and codes from the screen for the
next command. Browser steps record a separate video with Playwright.

Each run uses its own `HOME` under `/tmp/a2a-demo/you`, so it starts with no
pairings or contacts and leaves your real setup alone.

## Set up once (macOS)

```bash
brew install asciinema tmux agg ffmpeg node go
```

- asciinema 3 records without a terminal (`--headless`), so it runs from an
  agent's shell. asciinema 2 works from an interactive terminal only.
- Docker Desktop running, for the real agents.
- The `a2a` CLI v0.4.0 or later on PATH. `a2a-serve` on PATH, or pass
  `--build` to build it from this checkout.
- The model key exported, `OPENROUTER_API_KEY` by default. `DEMO_AGENT` and
  `DEMO_SECRET` change the agent and the key variable, and `DEMO_MODEL` the
  `--model` the agents start with (default
  `openrouter/~google/gemini-flash-latest`).
- For browser steps:

  ```bash
  cd tools/demo/browser && npm install && npx playwright install chromium
  ```

- For the Telegram steps, sign in to Telegram Web once. Recordings reuse the
  browser profile in `~/.cache/a2a-demo/browser-profile`:

  ```bash
  node tools/demo/browser/record.mjs --login https://web.telegram.org/k/
  ```

To use this skill from Claude Code, link it into your skills:

```bash
mkdir -p ~/.claude/skills && ln -s "$PWD/tools/demo" ~/.claude/skills/record-demo
```

## Record

```bash
tools/demo/record.sh list                          # scenarios and what they need
tools/demo/record.sh 01-agent-server --fake        # dry run: fakeacp, no Docker or API key
tools/demo/record.sh 01-agent-server --build --gif # real agents, local build, plus a GIF
```

| Scenario | Needs |
| --- | --- |
| `01-agent-server` | Docker and a model key. `--fake` works. |
| `02-tailscale-telegram-grok` | Tailscale with HTTPS and Funnel (example step 1), `CODER_BOT_TOKEN`, Telegram Web sign-in. Grok steps run when `GROK_WEBHOOK_URL` and `GROK_WEBHOOK_TOKEN` are set. No `--fake`. The first run asks you to approve the `coder` machine; later runs reuse it from `~/.cache/a2a-demo/tsnet-coder`. |
| `03-agentmesh-relay` | `--build` (Agent Mesh support), free port 8443. Installs `agentmesh-one` if missing. `--fake` works. |
| `04-observability-agent` | Docker, a model key, `jq`, and free ports 3000, 4317 and 4318. Runs `grafana/otel-lgtm` with generated telemetry off camera, and fetches the recipe from GitHub with your git credentials. No `--fake`. |
| `inspector` | Docker and a model key, plus the browser setup. `--fake` works. |
| `relay-cloud-run` | `--build`, `gcloud` signed in, and `DEMO_PROJECT`, a project where Cloud Run may serve the public and Cloud SQL may have a public IP. Creates billed resources named as in the guide, which you delete afterwards. A Cloud SQL instance name can't be reused for a week after deletion. Before a release, set `DEMO_RELAY_IMAGE` to an image repository you pushed the relay image to, tagged `v0.1.0`. `--fake` works. |

Output goes to `tools/demo/out/`: `<scenario>.cast`, and
`<scenario>-browser-<n>.webm` for each browser step. Other flags:
`--speed 2` types and pauses twice as fast, `--size 100x30` sets the terminal,
`--keep` keeps `/tmp/a2a-demo` for debugging. `asciinema rec -i 2` caps idle
time, so long model turns shrink to two seconds.

`--gif` renders `<scenario>.gif` with `compose.sh`. Each browser video is
spliced in where the terminal handed over to it, as noted in
`<scenario>.markers`, and the GIF plays at `--gif-speed` (default 1.5). Run
`tools/demo/compose.sh <scenario>.cast <out.gif>` again after you edit a cast
or trim a video.

The examples embed their demo as `demo.gif` next to the README, such as
`examples/01-agent-server/demo.gif`. Copy the reviewed GIF there.

When you run it as an agent:

- A real run takes several minutes. Start it as a background job and read
  its output when it finishes.
- Do a `--fake` run first when the scenario supports it. It checks the
  scenario in about a minute.
- Scenario 02 needs the user. Its output prints `ACTION NEEDED: ...` lines,
  such as approving the Tailscale machine. Relay each one to the user right
  away; the run waits up to 30 minutes.
- The user can watch live with `tmux -L a2a-demo attach -r`.

## Review before publishing

- Play it: `asciinema play tools/demo/out/<scenario>.cast`. Look at a video
  frame with `ffmpeg -ss 5 -i <file>.webm -frames:v 1 frame.png`.
- `record.sh` deletes a cast that shows the value of a model key, bot token
  or webhook variable. It can't see tailnet names, email addresses, chat
  names or one-time links. Check for them, and re-record or edit the cast if
  they shouldn't be public.
- Trim the start of a video, such as a frame of old chat history, with
  `ffmpeg -ss 1 -i in.webm -c:v libvpx-vp9 -b:v 0 -crf 32 -an out.webm`, and
  compose again.

## Write or fix a scenario

A scenario is a bash file that `record.sh` sources. It must run on bash 3.2,
the macOS default. It defines:

- `layout`: creates the panes with `panes`, `split_pane` and `title`.
- `scene`: the recorded steps.
- `prepare` (optional): off-camera setup before the recording starts.
- `teardown` (optional): runs after the agents stop and before the work
  directory is removed, to keep state for the next run.

Type the commands of the example README as written, in its order, so the
demo matches the docs. When a README changes, update its scenario. Use
`$AGENT` and `$AGENT_FLAGS` in start commands, so `--fake` can swap in
fakeacp.

Helpers from `lib.sh`, where `<pane>` is a pane name:

| Helper | Does |
| --- | --- |
| `panes <name>` | Creates the session with its first pane. |
| `split_pane <new> <from> above\|below\|left\|right <pct>` | Adds a pane next to another. |
| `title <pane> <text>` | Sets the label on the pane border. |
| `run <pane> <cmd> [timeout]` | Types the command, runs it and waits for the prompt. |
| `start <pane> <cmd>` | Types and runs a command that keeps running, such as `a2a-serve start`. |
| `answer <pane> <ERE> <reply>` | Waits for a command's question and types the reply; an empty reply takes the default. |
| `reply_pending <pane> <reply> [flags]`, `approve_pending <pane> [flags]` | Answers the approvals the last `a2a send` left open, such as with `reject`. Flags such as `-a <url>` are repeated on each send. |
| `say <pane> <text>` | Types a `# text` comment as narration. |
| `wait_text <pane> <ERE> [timeout]` | Waits until the pane shows a match. |
| `last_match <pane> <ERE>` | Prints the last match on the pane, to reuse a task ID or code. |
| `clear_pane`, `interrupt`, `pause <s>` | Ctrl-L, Ctrl-C, a pause scaled by `--speed`. |
| `until_ok <timeout> <cmd...>` | Retries a command off camera, for waits that would bore the viewer. |
| `fake <real> <fake>` | Picks a prompt that fakeacp understands in `--fake` mode (`hello`, `ask`, `call <agent> <text>`; see `internal/agentsrv/testdata/fakeacp/main.go`). |
| `browser <name> [key=value...]`, `browser_wait` | Records `browser/<name>.mjs` in the background, and waits for it. |
| `ask_human <msg> [<pane> <ERE> [timeout]]` | Asks the person running it to do something, and waits for the pane to show it is done. |

A browser scenario in `browser/` exports
`default async function ({ page, params, pause, type })`. `params` holds the
`key=value` arguments, and `type(locator, text)` types at human speed. On
failure, `record.mjs` saves `<output>-failure.png`; look at it to fix a
selector. The Telegram selectors target the Telegram Web K client and break
when Telegram changes its page.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `a demo is already running` | A run is still going, or one crashed. `tmux -L a2a-demo kill-server`. |
| `/tmp/a2a-demo exists` | Stop agents left in it with `a2a-serve stop --dir <dir>`, then remove it. |
| `no prompt after 600s` | The command hung or asked for input. The pane contents print on failure. |
| The tailnet machine is `coder-1` | A machine named `coder` is left from a run whose identity was not saved. Remove it in the Tailscale admin console, and delete `~/.cache/a2a-demo/tsnet-coder`. |
| `asciinema: ... not a tty` | asciinema 2 needs a terminal. Install asciinema 3, or run `record.sh` in a terminal. |
