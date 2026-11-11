# Talk to Your Agent on Telegram

This example sets up an agent that you talk to from Telegram with voice notes, and that answers with voice notes when you ask it to.

## What you'll learn

- How to connect the agent to a Telegram bot
- How to give the server a speech service: a hosted one with an API key, or one on your machine
- How voice notes reach the agent, and how to check what it heard
- How to turn voice replies on and off with `/voice`
- How to go back to an earlier conversation with `/conversations`
- How an A2A caller finds out that the agent takes audio

## Prerequisites

- The setup of [Serve a Coding Agent](../01-agent-server/README.md#prerequisites): the `a2a` CLI, `a2a-serve`, Docker, and an API key such as `OPENROUTER_API_KEY` exported in your shell. Without Docker, add `--sandbox local` to the `config set` command in step 2.
- A Telegram account.
- A speech service. Step 3 shows two ways: an OpenAI API key, or speaches running in Docker on your machine.

## 1. Create a Telegram bot

In Telegram, talk to [@BotFather](https://t.me/BotFather): send `/newbot`, choose a name and a username, and copy the token. Each agent needs its own bot.

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

`config set --agent` creates the agent without starting it, and `messenger add` connects the bot and stores its token.

## 3. Give the server a speech service

Pick one of the two ways.

### With an OpenAI API key

```bash
export OPENAI_API_KEY=sk-…
a2a-serve config set --voice openai --voice-key env:OPENAI_API_KEY
```

The agent doesn't get the key.

### On your machine, with speaches

[speaches](https://speaches.ai) runs in Docker, needs no account, and keeps audio on your machine. The server asks it for the models `whisper-1` and `tts-1`. A small file tells speaches which models those names mean. Whisper base transcribes quickly without a GPU, and Kokoro speaks:

```bash
cat > ~/a2a-example6/speaches-models.json <<'EOF'
{"whisper-1": "Systran/faster-whisper-base", "tts-1": "speaches-ai/Kokoro-82M-v1.0-ONNX"}
EOF
chmod 644 ~/a2a-example6/speaches-models.json
```

Start speaches, wait until it answers, and download both models:

```bash
docker run -d --name speaches -p 127.0.0.1:8000:8000 \
  -v speaches-models:/home/ubuntu/.cache/huggingface/hub \
  -v ~/a2a-example6/speaches-models.json:/home/ubuntu/speaches/model_aliases.json:ro \
  ghcr.io/speaches-ai/speaches:latest-cpu
until curl -sf http://localhost:8000/health >/dev/null; do sleep 1; done
curl -X POST http://localhost:8000/v1/models/Systran/faster-whisper-base
curl -X POST http://localhost:8000/v1/models/speaches-ai/Kokoro-82M-v1.0-ONNX
a2a-serve config set --voice http://localhost:8000/v1
```

The models stay in the `speaches-models` volume, so a new container doesn't download them again. With a GPU, a larger model such as `Systran/faster-whisper-large-v3` hears more accurately. For a male voice, add `--voice-name am_michael`. The server calls speaches from your machine, so `localhost` works with the Docker sandbox too.

## 4. Check the speech service

```bash
a2a-serve doctor --online
```

The check speaks a short sample and transcribes it back:

```
  ✓ voice service openai transcribes and speaks
```

A failure says what failed, with a fix:

```
  ✗ voice service openai can't speak: the speech service refused the key
      fix: a2a-serve config set --voice-key - --restart
```

With speaches, the line shows its URL.

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

The first reply is what the server heard. If it heard you wrong, send the note again or type the message.

## 7. Turn voice replies on

```
you → /voice on
bot → Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.
you → 🎤 "Which idea is the quickest to build?"
bot → 🎤 "Which idea is the quickest to build?"
bot → The recipe scaler: one page, no backend.
      ▶️ 0:04
```

With voice on, every answer comes as text and a voice note, also when you type. Code, tables and links stay in the text. `/voice off` goes back to text replies. The setting is yours, and other people keep their own.

## 8. Go back to an earlier conversation

`/new` starts a new conversation, and `/conversations` takes you back to an earlier one:

```
you → /new
bot → Started a new conversation. /conversations lists the earlier ones.
you → What's a good name for a recipe app?
bot → Pinch.
you → /conversations
bot → Your recent conversations here:
      [ Now: 1 turn, "What's a good name for a recip…" ]
      [ 5 minutes ago: 2 turns, voice note ]
you → (taps "5 minutes ago")
bot → Switched to the conversation from 5 minutes ago.
```

The agent continues the conversation about ideas.md.

## 9. Check the agent card

A2A callers can send audio too. In a second terminal, from the agent's directory, print the audio types on the agent card:

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

[Audio Messages](../../docs/reference/audio-messages.md) describes how callers send audio and ask for spoken replies.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| `I couldn't transcribe the voice note: the speech service timed out` | On speaches without a GPU, long voice notes take a while, more so with larger models. Send shorter notes, keep `whisper-1` on `Systran/faster-whisper-base` in `speaches-models.json` (step 3), or use a hosted service. |
| `I couldn't transcribe the voice note: couldn't reach the speech service` | The service isn't running or its URL is wrong. With speaches, check `docker ps` and the URL in `a2a-serve config show`. |
| Answers come as text with `(The voice reply failed: the speech service doesn't speak.)` | The service has no speech endpoint or model. With speaches, download the speech model (step 3). |
| `/voice on` says voice replies aren't set up | The agent has no `--voice` setting, or it applies after `a2a-serve restart`. Run `a2a-serve doctor`. |
| A voice reply arrives as a file | Telegram delivers a voice note as a file when your privacy settings block voice notes. |
| Telegram is silent | Only one process can use a bot token; stop any other process using it. |

## Next steps

[Tailscale, Telegram and a Grok Bot](../02-tailscale-telegram-grok/README.md) makes the agent reachable from your tailnet and keeps it running as a service. The user guide covers [voice](../../docs/guide/voice.md), [Discord and email](../../docs/guide/messengers.md), and [approvals](../../docs/guide/approvals.md).
