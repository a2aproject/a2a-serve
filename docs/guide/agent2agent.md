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
- With `a2a send --async`, the agent moves on, and the result arrives later in the same conversation. A result over 16 KiB arrives as the file `result.txt`, with its first lines in the message.
- A response from a contact can be at most 32 MiB. Streams have no limit.
- A contact on a public URL can't point the server into your private network (see [Security](../advanced/security.md#calls-to-other-agents-and-webhooks)).
- When the called agent asks for approval, the question comes to you in your chat, and your answer goes back to it.
- A chain of calls stops after 3 hops, so agents can't call each other in a loop.
