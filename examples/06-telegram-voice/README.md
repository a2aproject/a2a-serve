# Talk to Your Agent on Telegram

This example sets up an agent that you talk to from Telegram with voice notes, and that answers with voice notes when you ask it to.

## What you'll learn

- How to connect the agent to a Telegram bot
- How to give the server a speech service: a hosted one with an API key, or one on your machine
- How voice notes reach the agent, and how to check what it heard
- How to turn voice replies on and off with `/voice`
- How an A2A caller finds out that the agent takes audio

## Prerequisites

- The setup of [Serve a Coding Agent](../01-agent-server/README.md#prerequisites): the `a2a` CLI, `a2a-serve`, Docker, and an API key such as `OPENROUTER_API_KEY` exported in your shell. Without Docker, add `--sandbox local` to the `config set` command in step 2.
- A Telegram account.
- A speech service. Step 3 shows two ways: an OpenAI API key, or speaches running in Docker on your machine.

## 1. Create a Telegram bot

In Telegram, talk to [@BotFather](https://t.me/BotFather): send `/newbot`, choose a name and a username, and copy the token it gives you. Each agent needs its own bot, because only one process can poll a bot token.

```bash
export CODER_BOT_TOKEN=<token from BotFather>
```

## 2. Create the agent

```bash
mkdir -p ~/a2a-example6/coder && cd ~/a2a-example6/coder
a2a-serve config set --agent opencode --name coder --secret OPENROUTER_API_KEY \
  --model openrouter/~google/gemini-flash-latest
a2a-serve messenger add telegram --token env:CODER_BOT_TOKEN
```

`config set --agent` creates the agent without starting it, and `messenger add` connects the bot. `env:CODER_BOT_TOKEN` reads the token from your shell now and stores it in `.a2a/state/secrets`, so later runs don't need the variable.

## 3. Give the server a speech service

The server turns voice notes into text before the agent sees them, and turns the agent's replies into voice notes. Pick one of the two ways.

### With an OpenAI API key

```bash
export OPENAI_API_KEY=sk-…
a2a-serve config set --voice openai --voice-key env:OPENAI_API_KEY
```

The server stores the key in `.a2a/state/secrets` and uses it only for speech. The agent doesn't get it.

### On your machine, with speaches

[speaches](https://speaches.ai) is a speech server that runs in Docker. Audio stays on your machine, and you need no account. Start it and download one model that transcribes and one that speaks:

```bash
docker run -d --name speaches -p 8000:8000 ghcr.io/speaches-ai/speaches:latest-cpu
export SPEACHES_BASE_URL=http://localhost:8000
uvx speaches-cli model download Systran/faster-whisper-large-v3
uvx speaches-cli model download speaches-ai/Kokoro-82M-v1.0-ONNX
a2a-serve config set --voice http://localhost:8000/v1
```

The server asks speaches for the OpenAI model names `whisper-1` and `tts-1`, and speaches maps them to these two models. To hear a male voice, add `--voice-name am_michael`; the [voice guide](../../docs/guide/voice.md#pick-a-voice) lists more. The server calls speaches from your machine, so `localhost` works with the Docker sandbox too.

## 4. Check the speech service

```bash
a2a-serve doctor --online
```

The check speaks a short sample and transcribes it back. Look for the voice line:

```
  ✓ voice service openai transcribes and speaks
```

When it fails, the line says which direction failed and why, with a fix:

```
  ✗ voice service openai can't speak: the speech service refused the key
      fix: a2a-serve config set --voice-key -
```

With speaches, the line shows its URL. The agent isn't running yet, so `doctor` also notes that.

## 5. Start the agent and link Telegram

```bash
a2a-serve start
```

```
...
✓ voice    openai
✓ local    http://127.0.0.1:46757
✓ telegram @coder_agent_bot
→ link yourself as owner: https://t.me/coder_agent_bot?start=K7Q29XF4   (15 min)
```

Open the `t.me` link on your phone and press **Start**. The bot replies:

```
Linked as owner. Write to talk to coder.
```

## 6. Send a voice note

Hold the microphone button in the chat and say: "Create a file ideas.md with three ideas for a weekend project."

```
you → 🎤 0:05
bot → 🎤 "Create a file ideas.md with three ideas for a weekend project."
bot → Created ideas.md with three ideas: a plant watering reminder, a recipe scaler, and a board game score keeper.
```

The first reply is what the server heard. If it heard you wrong, send the note again or type the message. When the agent asks before a tool call, the approval buttons work as for typed messages.

## 7. Turn voice replies on

```
you → /voice on
bot → Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.
you → 🎤 "Which idea is the quickest to build?"
bot → 🎤 "Which idea is the quickest to build?"
bot → The recipe scaler: one page, no backend.
      ▶️ 0:04
```

With voice on, every answer comes as text and a voice note, also when you type. The agent can write a short spoken version of a long answer, which the server reads aloud and leaves out of the text. Otherwise the server speaks the prose of the answer. Code, tables and links stay in the text, and a long answer is cut at about 1,000 characters. Questions and approvals stay text-only, because they have buttons. `/voice off` goes back to text replies, and `/voice` alone shows the current state. The setting is yours. Other people who chat with the agent keep their own.

`/help` lists all the commands.

## 8. Check the agent card

A2A callers can send audio too. In a second terminal, from the agent's directory, print the audio types that the agent card lists:

```bash
a2a-serve card | grep -A6 defaultInputModes
```

```
  "defaultInputModes": [
    "text/plain",
    "application/octet-stream",
    "audio/ogg",
    "audio/mpeg",
    "audio/wav"
  ],
```

A caller that lists `audio/ogg` in `acceptedOutputModes` gets the reply as audio too. See [Agent2Agent](../../docs/guide/agent2agent.md#audio).

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `I couldn't transcribe the voice note: the speech service refused the key` | The key of the service. Set it again with `a2a-serve config set --voice-key env:OPENAI_API_KEY --restart`. |
| `I couldn't transcribe the voice note: the speech service timed out` | On speaches without a GPU, long voice notes take a while. Send shorter notes, or use a hosted service. |
| `I couldn't transcribe the voice note: couldn't reach the speech service` | The service isn't running or its URL is wrong. With speaches, check `docker ps` and the URL in `a2a-serve config show`. |
| Answers come as text with `(The voice reply failed: the speech service doesn't speak.)` | The service has no speech endpoint or model. With speaches, download the speech model (step 3). |
| `/voice on` says voice replies aren't set up | The agent has no `--voice` setting, or it applies after `a2a-serve restart`. Run `a2a-serve doctor`. |
| A voice reply arrives as a file | Telegram delivers a voice note as a file when your privacy settings block voice notes. |
| The voice note is silent or unclear | The speech service heard no words. Record it again. |
| Telegram is silent | Only one process can use a bot token; stop any other process using it. |

## Next steps

[Tailscale, Telegram and a Grok Bot](../02-tailscale-telegram-grok/README.md) makes the agent reachable from your tailnet and keeps it running as a service. The user guide covers [voice](../../docs/guide/voice.md), [Discord and email](../../docs/guide/messengers.md), and [approvals](../../docs/guide/approvals.md).
