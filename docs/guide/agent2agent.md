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
- With `a2a send --async`, the agent moves on, and the result arrives later in the same conversation. If the result doesn't come, the agent gets a message that says so. See [limits](../reference/limits.md#agent-calls).
- When you cancel a task, the server cancels the tasks that the agent started on other agents for it.
- When the called agent asks for approval, the question comes to you in your chat, and your answer goes back to it. If no chat can take the question, it arrives in your conversation as a new task in `input-required`. Answer it with `a2a send --task-id`.
- A chain of calls has a hop limit, so agents can't call each other in a loop. See [limits](../reference/limits.md#agent-calls).

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

Edit them with `a2a-serve config edit`. A running agent serves the new card at once. The [configuration reference](../reference/configuration.md#card-skills) lists the rules.

## Audio

With a [speech service](voice.md) set up, callers can send recordings, and the agent gets them as text. A caller that lists `audio/ogg` in `acceptedOutputModes` also gets the reply as a voice note. [Audio Messages](../reference/audio-messages.md) describes the parts.

## Cards of other agents

`a2a-serve contact add` fetches the contact's agent card, keeps its description and skills, and prints them. The server fetches the cards again regularly, and refreshes them now with `a2a-serve contact refresh [name]`. Change the period with `a2a-serve config set --contact-refresh 30m`.

The agent's instructions and the agents line at the start of each conversation list each agent with its description and skills. They change after a refresh. Agents that leave the private network drop out at the next refresh. The server shortens the text of other agents' cards before the agent reads it.

## Peer names

`a2a-serve peers` lists each agent by the name in its agent card while that name is unique on the private network. Commands such as `a2a-serve contact add <name>` and `<pairing>@<agent>` use the listed names.

- When another agent's card has the same name, the agent is listed by its network name. That is its machine name on the tailnet, or the name it joined the relay with.
- When agents share a network name, each one gets a suffix derived from its machine, such as `bob-1a2b3c4d`.
