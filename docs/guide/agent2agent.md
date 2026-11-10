# Agent2Agent

The agent calls your contacts and webhook nodes with the `a2a` CLI. The server makes the calls with its own credentials, so tokens never enter the sandbox.

```sh
a2a-serve peers                     # agents on the private network
a2a-serve contact add coder         # a peer on the tailnet: no code needed
a2a-serve contact add grok --url https://grok.example.com --code K7Q2-9XF4
a2a-serve contact list
a2a-serve contact refresh           # fetch the contacts' agent cards again
a2a-serve contact rm coder
```

To let the agent call every agent on the private network without adding each one, run `a2a-serve config set --private-allow-outgoing everyone`. It calls them by name with its network identity, and the agents line at the start of each conversation lists them. [Access Model](../advanced/access-model.md#whom-the-agent-may-call) explains who can make it call them.

- Each prompt tells the agent which agents it can call, with the description and skills from each agent's card. See [Cards of other agents](#cards-of-other-agents).
- With `a2a send --async`, the agent moves on, and the result arrives later in the same conversation. A result over 16 KiB arrives as the file `result.txt`, with its first lines in the message. The server watches the task for at most 7 days. It also stops when the task keeps the same state for 24 hours. The 24-hour limit pauses while the task waits for your approval. Either way, the agent gets a message that says so.
- When the agent is too busy to take a result, such as when you have 8 unfinished tasks, the server posts it again at the next poll until the agent takes it. An agent on the private network that is offline counts as unreachable. The server keeps polling it for a day, and then the agent gets a message that says so.
- When you cancel a task, the server cancels the tasks that the agent started on other agents while it worked on that task. A task that the server can't cancel stays watched, and its result still arrives in the conversation.
- A response from a contact can be at most 32 MiB. Streams have no limit.
- A contact on a public URL can't point the server into your private network (see [Security](../advanced/security.md#calls-to-other-agents-and-webhooks)).
- When the called agent asks for approval, the question comes to you in your chat, and your answer goes back to it. If you talk to the agent with `a2a send` and no messenger chat can take the question, it arrives in your conversation as a new task in `input-required`. Answer it with `a2a send --task-id`. If the question is answered elsewhere, such as in the inspector, that task completes. After you answer, that task waits for the called agent and continues with its result. If the result can't come because you removed the contact or the user lost access, the waiting task fails with a message that says why.
- A chain of calls stops after 3 hops, so agents can't call each other in a loop.

## Your agent's card

Callers read the agent card to learn what the agent does. `a2a-serve card` prints it as callers see it. Each URL serves its own card, so the command takes `--url=local|private|public` like `a2a-serve status`.

The card lists one skill, "Coding agent", until you list skills under `card.skills` in `config.yaml`:

```yaml
card:
  skills:
    - id: release-notes
      name: Release notes
      description: Writes release notes from merged pull requests.
      tags: [git, docs]
```

Edit them with `a2a-serve config edit`. A running agent serves the new card at once. The [configuration reference](../reference/configuration.md#card-skills) lists the rules. The server doesn't read skills from the project's skill folders, because the agent can write those files.

## Audio

When a [speech service](voice.md) is set up, callers can send audio and ask for audio back. The agent card then lists `audio/ogg`, `audio/mpeg` and `audio/wav` in `defaultInputModes`, and `audio/ogg` in `defaultOutputModes`.

A message with an `audio/*` file part that holds raw bytes is transcribed before the agent sees it. The transcript arrives as the first `working` status of the task. Its message text is `🎤 "<transcript>"`, and its metadata has the transcript under the key `a2a-serve/transcript`.

A caller that lists `audio/ogg` in the `acceptedOutputModes` of its request gets the reply as an extra artifact named "Voice reply", before the task completes. The artifact has one file part, `reply.ogg` with the type `audio/ogg`. A caller that accepts only `audio/mpeg` or `audio/wav` gets `reply.mp3` or `reply.wav`. `audio/*` is accepted too. The speech calls of a caller count against the [limits](voice.md#limits) of that caller.

## Cards of other agents

`a2a-serve contact add` fetches the contact's agent card, keeps its description and skills, and prints them. The server fetches the cards of contacts again every hour, and the cards of the agents on the private network too. Change the period with `a2a-serve config set --contact-refresh 30m`, or refresh the contacts now with `a2a-serve contact refresh [name]`.

The agent's instructions and the agents line at the start of each conversation list each agent with its description and skills. They change only after a refresh, and a change rewrites the instruction files. Agents that leave the private network drop out at the next refresh.

A card comes from the other agent, so the server bounds its text before the agent reads it. It keeps at most 10 skills and short descriptions, removes line breaks and markdown, and quotes the text as the agent's own description of itself.

## Peer names

`a2a-serve peers` lists each agent by the name in its agent card while that name is unique on the private network. Commands such as `a2a-serve contact add <name>` and `<pairing>@<agent>` use the listed names. The server keeps each peer's card for 10 minutes, so a changed card name shows up within 10 minutes. A machine that doesn't serve an agent is checked again after a minute.

- When another agent's card has the same name, or another machine has it as its network name, the agent is listed by its own network name. That is its machine name on the tailnet, or the name it joined the relay with.
- When agents share a network name, each one gets a suffix derived from its machine, such as `bob-1a2b3c4d`.
