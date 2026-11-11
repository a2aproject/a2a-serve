# Voice

People can send the agent voice notes and get voice notes back, through a speech service that the owner picks.

The server does the speech work. It turns each voice note into text before the agent sees it, and it turns the agent's answers into voice notes when the person asks for them. This works with every agent and sandbox, in [Telegram, Discord and email](messengers.md), and for [A2A callers](agent2agent.md#audio).

## Pick a speech service

One setting, `--voice`, names the service, and `--voice-key` gives its API key. Both are settings of [`a2a-serve config set`](../reference/commands/config.md) that only owners can change, and they apply after `a2a-serve restart`.

| `--voice` | Who it suits | Key | Transcribes | Speaks |
|---|---|---|---|---|
| `openai` | Owners with an OpenAI account | `--voice-key` is required | yes | yes, as Ogg Opus |
| A URL | Owners who run a server with the OpenAI audio API, or use another hosted service with that API | `--voice-key` if the service needs one | yes | when the server has `/audio/speech` |
| `local` (planned) | Owners with Docker who want no account | none | yes | yes |
| `gemini` (planned) | Owners whose agent uses Gemini | `--voice-key` | yes | yes |

### A hosted service

```sh
export OPENAI_API_KEY=sk-…
a2a-serve config set --voice openai --voice-key env:OPENAI_API_KEY
```

The server uses the models `gpt-4o-mini-transcribe` and `gpt-4o-mini-tts`, with the voice `alloy` unless you [pick another](#pick-a-voice).

### Your own server

Any server with the OpenAI audio API shape works, such as [speaches](https://speaches.ai) on your machine or on the private network. The API has `/audio/transcriptions` and `/audio/speech`. Give the URL up to its `/v1`:

```sh
a2a-serve config set --voice http://localhost:8000/v1
```

A URL with a user name and password is refused. Put the key in `--voice-key`.

The server asks a URL service for the models `whisper-1` and `tts-1` and the voice `alloy`, which are OpenAI's names. speaches maps `whisper-1` to `Systran/faster-whisper-large-v3` and `tts-1` to `speaches-ai/Kokoro-82M-v1.0-ONNX`, and uses its default voice unless you [pick another](#pick-a-voice). Run speaches in Docker and download the two models:

```sh
docker run -d --name speaches -p 8000:8000 ghcr.io/speaches-ai/speaches:latest-cpu
export SPEACHES_BASE_URL=http://localhost:8000
uvx speaches-cli model download Systran/faster-whisper-large-v3
uvx speaches-cli model download speaches-ai/Kokoro-82M-v1.0-ONNX
```

A service without `/audio/speech` only transcribes. Voice notes work, and voice replies fall back to text with the note `(The voice reply failed: the speech service doesn't speak.)`.

### Pick a voice

`--voice-name` picks the voice of the spoken replies, by a name the service has:

```sh
a2a-serve config set --voice-name nova
```

OpenAI has about a dozen voices, such as `alloy`, `echo`, `nova`, `onyx` and `shimmer`, and doesn't label them by gender. Kokoro, the speech model of speaches, names its voices by language and gender: `af_heart` is an American female voice, `am_michael` an American male one, and `bf_emma` a British female one. speaches replaces OpenAI's voice names with its default voice, so give it a Kokoro name.

The server can't check the name. `a2a-serve doctor --online` speaks with the voice and reports a name the service refuses, such as `the speech service refused the request: Invalid value: 'robot'…`. `a2a-serve config unset voice-name` goes back to the service's voice.

The voice applies to everyone who talks to the agent.

### The key

`--voice-key` takes the [value forms](secrets.md#value-forms) of the server's other tokens. The server stores the key in `.a2a/state/secrets`, and the agent never gets it. See [Secrets](secrets.md#tokens-for-the-server).

A `--voice` on another host clears the key, so that the key goes only to the service it was issued for. Pass `--voice-key` in the same command to give the new service its key.

### Check it

`a2a-serve doctor` shows that voice is set up. `a2a-serve doctor --online` speaks a short sample and transcribes it back:

```console
$ a2a-serve doctor --online
...
  ✓ voice service openai transcribes and speaks
```

A service that doesn't speak shows `✓ voice service http://localhost:8000/v1 transcribes; it doesn't speak, so replies stay text`. A failure names the direction that failed and prints a fix:

```console
  ✗ voice service openai can't speak: the speech service refused the key
      fix: a2a-serve config set --voice-key - --restart
```

When the server starts, it prints `✓ voice    openai`, or the URL of the service. The [agent card](agent2agent.md#audio) then lists the audio types.

### Turn voice off

`a2a-serve config unset voice` removes the service, its key and the voice name:

```sh
a2a-serve config unset voice --restart
```

Audio attachments then reach the agent as files.

## Talk to the agent

Send a voice note or an audio file in the chat. The server transcribes it, and the agent gets only the text. An image that comes with the voice note still reaches the agent. The task history keeps the audio.

On Telegram and Discord, the server replies to the voice note with what it heard, so that you can check it:

```
you → 🎤 0:05
bot → 🎤 "Fix the flaky parser test."
bot → The test raced on a shared buffer. I gave each case its own buffer.
```

Email gets only the result.

A voice note can answer a question of the agent. An approval takes a button or a typed option, because a misheard answer could allow a tool call.

Without a speech service, audio attachments reach the agent as files.

## Voice replies

Voice replies are off until you turn them on. `/voice on` and `/voice off` turn them on and off, and `/voice` alone shows the current state:

```
you → /voice on
bot → Voice replies are on. I'll send a voice note with each answer. /voice off turns them off.
```

With voice on, each completed reply arrives as text plus a voice note, whether you typed or spoke. Questions and approvals stay text-only, because they have buttons. In Discord, `/voice` offers `on` and `off` as a choice. In email, write the command as the text of the message.

The setting belongs to you and applies in all your chats with the agent. `a2a-serve pair rm` removes it with your pairing. An email sender on the `--allow-email` list keeps it until 90 days pass without a message. A guest, who joined without a code, can't turn it on.

When voice isn't set up, members get `Voice replies aren't set up on this agent. Ask its owner to set them up.` The owner gets the command to run: `a2a-serve config set --voice <service>`.

### What is spoken

When a reply is spoken, the server tells the agent so in that turn. The agent can then end its reply with a short spoken version in a `<speak>` block:

```
The test raced on a shared buffer. I gave each case its own buffer:

    buf := new(bytes.Buffer)

<speak>The test shared a buffer between cases. Each case has its own now, and the race is gone.</speak>
```

The server reads the block aloud and takes it out of the text, so the chat shows the reply without it. A block starts with `<speak>` at the start of a line and ends with `</speak>` at the end of a line. A `<speak>` inside a sentence, inline code or a code block stays in the text, and so does a block that never ends. The server removes blocks from every reply, also when nobody asked for a voice reply. A reply that is only a block shows the block as text when it isn't spoken. Answers to the agent's questions reach the agent as written, without the note.

Without a block, the server speaks the prose of the reply. It leaves out code blocks, tables, links and markup, so those stay in the text. It cuts a long reply at about 1,000 characters and adds "The rest is in the text." A `<speak>` block is cut at about 4,000 characters, about the most the services speak at once.

- **Telegram:** the voice note plays inline. If you block voice notes in your privacy settings, it arrives as a file.
- **Discord and email:** the voice note is an audio attachment, `reply.ogg`.

## When speech fails

A voice note that can't be transcribed fails the task before the agent runs. A voice note that answers a question of the agent leaves the question waiting, so that you can answer again:

```
bot → I couldn't transcribe the voice note: the speech service timed out. Send it again or type it.
```

| Reason in the message | What it means |
|---|---|
| `the speech service timed out` | The service took longer than 60 seconds. |
| `the speech service refused the key` | The key is wrong or expired. The owner can fix it with `a2a-serve config set --voice-key - --restart`. |
| `couldn't reach the speech service` | The service is down or the URL is wrong. The owner can check with `a2a-serve doctor --online`. |
| `the recording is longer than 5 minutes` | See [Limits](#limits). |
| `the speech service heard no words` | The recording was silent or unclear. |
| `the recording isn't Ogg Opus, MP3 or WAV` | See [Limits](#limits). |
| `the speech service is over its rate limit or quota` | The service refused the call because the account used up its limit. |
| `you reached the limit of 60 speech calls an hour` | See [Limits](#limits). |
| `guests used up their 60 speech calls this hour; try again later` | Guests share one limit. See [Limits](#limits). |
| `too many people use voice right now; try again later` | Many people used voice within the same hour. Try again later. |

When speaking a reply fails, the text still arrives, followed by a note such as `(The voice reply failed: the speech service timed out.)`.

## Limits

| Limit | Value |
|---|---|
| Size of a recording | 25 MiB, and 20 MiB to download from Telegram |
| Length of a recording | 5 minutes |
| Format of a recording | Ogg Opus, MP3 or WAV, the formats whose length the server reads; owners can also send other formats |
| One call of the speech service | 60 seconds |
| Recordings in one message | 10 |
| Speech calls per person | 60 an hour, transcriptions and replies together; guests on each messenger and on the public URL share one limit, and owners have none |
