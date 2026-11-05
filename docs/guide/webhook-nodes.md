# Webhook Nodes

A webhook node is an agent that runs only when woken, such as a Grok bot routine. The server gives it an A2A address, holds its tasks and wakes it with a signed POST to its webhook:

```sh
a2a-serve pair invite grok --webhook https://<grok routine webhook URL> \
  --webhook-header "Authorization: Bearer env:GROK_WEBHOOK_TOKEN"
# → a message for the agent behind the webhook: its instructions and a pairing code
a2a-serve pair invite grok          # a new code, if the node lost its token
a2a-serve pair set grok --webhook https://<new URL>
a2a-serve pair set grok --rotate    # revoke its token and print a new code
a2a-serve pair get grok             # webhook, headers, pending tasks
a2a-serve pair rm grok
```

The node reads its instructions at `<agent-url>/mailbox/skill.md` and answers with `a2a-serve mailbox reply <task> --final "…"`. A task with no reply for 10 minutes fails.

Give every webhook a credential that only the server sends, such as a `--webhook-header` token or a secret in the webhook URL. Anyone who learns an unprotected webhook URL can send the node a wake. A node that runs code on each wake can also check it with `a2a-serve mailbox verify`, which needs only the node token.

Your agent calls the node as `grok`. You can call it at `<agent-url>/n/grok`. The node can also call your agent with its token, as an authenticated caller.

The server follows redirects of the webhook. It sends the webhook headers and the signature only to the host of the webhook URL, and drops them when a redirect leads to another host. A wake that takes longer than 20 seconds fails and is retried. [Security](../advanced/security.md#calls-to-other-agents-and-webhooks) lists the redirects the server refuses.

## Advertised pairings

With `--advertised`, every agent on the private network can find a webhook node and call it through its host:

```sh
a2a-serve pair invite grok --webhook https://… --advertised   # on Ben's agent
a2a-serve peers                                               # on Anna's agent: lists grok@ben
a2a-serve contact add grok@ben                                # no code needed
```

Anna's calls go through Ben's agent, which keeps the webhook's credentials. Ben's agent checks Anna like any other caller, so she needs access to it (see [Identities](identities.md)). An advertisement spreads to all agents within a few minutes; removing the node or passing `--advertised=false` withdraws it the same way. An agent advertises at most 100 pairings. A call to `<pairing>@<agent>` goes to the agent that `a2a-serve peers` lists under that [name](agent2agent.md#peer-names).

Each caller other than you and your agent can have at most 20 tasks waiting for the node at a time, and these callers together at most 100. A call past the limit fails until one of the caller's tasks finishes or asks a question.
