# Agent2Agent

The agent calls your contacts and webhook nodes with the `a2a` CLI. The server makes the calls with its own credentials, so tokens never enter the sandbox.

```sh
a2a-serve peers                     # agents on the private network
a2a-serve contact add coder         # a peer on the tailnet: no code needed
a2a-serve contact add grok --url https://grok.example.com --code K7Q2-9XF4
a2a-serve contact list
a2a-serve contact rm coder
```

- Each prompt tells the agent which agents it can call.
- With `a2a send --async`, the agent moves on, and the result arrives later in the same conversation.
- When the called agent asks for approval, the question comes to you in your chat, and your answer goes back to it.
- A chain of calls stops after 3 hops, so agents can't call each other in a loop.

## Peer names

`a2a-serve peers` lists each agent by the name in its agent card while that name is unique on the private network. Commands such as `a2a-serve contact add <name>` and `<pairing>@<agent>` use the listed names.

- When another agent's card has the same name, or another machine has it as its network name, the agent is listed by its own network name. That is its machine name on the tailnet, or the name it joined the relay with.
- When agents share a network name, each one gets a suffix derived from its machine, such as `bob-1a2b3c4d`.
