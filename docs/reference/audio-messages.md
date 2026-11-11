# Audio Messages

This page describes how A2A callers send audio to an agent with a speech service and get spoken replies. [Voice](../guide/voice.md) covers how to set up the service.

## Agent card

With a speech service, the agent card lists `audio/ogg`, `audio/mpeg` and `audio/wav` in `defaultInputModes`, and `audio/ogg` in `defaultOutputModes`.

## Recordings

A message part with raw bytes and an `audio/*` media type is a recording. The server transcribes it before the agent runs, and the agent gets the transcript in its place. The task history keeps the audio.

The first `working` status of the task shows the transcript:

| Field | Content |
| --- | --- |
| Message text | `🎤 "<transcript>"`, shortened for a long transcript |
| Metadata `a2a-serve/transcript` | The whole transcript |

A recording that can't be transcribed fails the task with a status message that says why. A recording that answers a question of the agent asks the question again.

## Spoken replies

A request whose `acceptedOutputModes` include an audio type gets the reply spoken. Before the task completes, the server adds an artifact named "Voice reply" with one file part:

| Accepted type | File part |
| --- | --- |
| `audio/ogg` or `audio/*` | `reply.ogg`, `audio/ogg` |
| `audio/mpeg` | `reply.mp3`, `audio/mpeg` |
| `audio/wav` | `reply.wav`, `audio/wav` |

The `gemini` service speaks `audio/ogg` and `audio/wav`. The file part has the metadata `a2a-serve/voice-reply: true`. When the reply can't be spoken, the artifact is a text part that says why.

The server takes `<speak>` blocks out of the reply's text in every task, and speaks the last one in place of the prose of the reply.
