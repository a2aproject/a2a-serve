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

Your agent calls the node as `grok`. You can call it at `<agent-url>/n/grok`. The node can also call your agent with its token, as an authenticated caller.

## Advertised pairings

With `--advertised`, every agent on the private network can find a webhook node and call it through its host:

```sh
a2a-serve pair invite grok --webhook https://… --advertised   # on Ben's agent
a2a-serve peers                                               # on Anna's agent: lists grok@ben
a2a-serve contact add grok@ben                                # no code needed
```

Anna's calls go through Ben's agent, which keeps the webhook's credentials. Anna's login needs a role on Ben's agent, through `--identity` or the Tailscale app capability. An advertisement spreads to all agents within a few minutes; removing the node or passing `--advertised=false` withdraws it the same way.
