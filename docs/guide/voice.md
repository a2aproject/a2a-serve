# Voice

People can send the agent voice notes and get voice notes back, through a speech service that you pick. The server turns voice notes into text before the agent sees them, so this works with every agent, in [messengers](messengers.md) and for [A2A callers](agent2agent.md#audio).

## Set up a speech service

With an OpenAI API key:

```sh
a2a-serve config set --voice openai --voice-key env:OPENAI_API_KEY --restart
```

With the Gemini API key of your agent, stored as the [secret](secrets.md) `GEMINI_API_KEY`:

```sh
a2a-serve config set --voice gemini --voice-key secret:GEMINI_API_KEY --restart
```

`--voice-key env:GEMINI_API_KEY` takes the key from the environment.

With a server that has the OpenAI audio API, such as [speaches](https://speaches.ai) on your machine, give its URL up to `/v1`:

```sh
a2a-serve config set --voice http://localhost:8000/v1 --restart
```

[Talk to Your Agent on Telegram](../../examples/06-telegram-voice/README.md#on-your-machine-with-speaches) shows how to run speaches. A server without speech synthesis only transcribes, and replies stay text.

The agent never gets the key. See [Secrets](secrets.md#tokens-for-the-server).

`a2a-serve doctor --online` speaks a short sample and transcribes it back:

```console
$ a2a-serve doctor --online
...
  ✓ voice service openai transcribes and speaks
```

`a2a-serve config unset voice --restart` turns voice off.

### Pick a voice

```sh
a2a-serve config set --voice-name nova --restart
```

OpenAI has voices such as `alloy`, `echo`, `nova` and `onyx`. Gemini has voices such as `Kore`, `Puck`, `Charon` and `Aoede`. speaches uses Kokoro names, such as `af_heart` for an American female voice and `am_michael` for an American male one. The voice applies to everyone who talks to the agent. `a2a-serve doctor --online` reports a name the service doesn't have.

## Send voice notes

Send a voice note or an audio file in the chat. On Telegram and Discord, the bot first shows what it heard:

```
you → 🎤 0:05
bot → 🎤 "Fix the flaky parser test."
bot → The test raced on a shared buffer. I gave each case its own buffer.
```

If it heard you wrong, send the note again or type the message. Approvals take a button or a typed answer.

## Get voice replies

```
you → /voice on
bot → Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.
```

With voice on, each answer arrives as text plus a voice note, also when you type. Questions and approvals stay text. `/voice off` turns voice replies off, and `/voice` shows whether they are on. The setting is yours and applies in all your chats with the agent. In email, send the command as the text of a message.

The server speaks the prose of the answer and leaves code, tables and links in the text. The agent can also end its answer with a short spoken version in a `<speak>` block on its own lines. The server reads the block aloud and takes it out of the text:

```
The test raced on a shared buffer. I gave each case its own buffer:

    buf := new(bytes.Buffer)

<speak>The test shared a buffer between cases. Each case has its own now.</speak>
```

When a reply can't be spoken, the text still arrives with a note that says why. [Limits](../reference/limits.md#voice) lists the length of recordings and how many speech calls each person can make.
