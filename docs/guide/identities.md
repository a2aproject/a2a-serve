# Identities

The server recognizes each caller and gives it a role: owner, authenticated or anonymous.

`a2a-serve status` says who can call right now:

```
  access   you: the local URL and your other tailnet devices
           everyone on the tailnet, emails matching *@example.com, 2 pairings
           the public URL needs a pairing or Google sign-in
```

| Caller | How it is recognized | Role |
| --- | --- | --- |
| You, on the machine running the agent | The token a2a-serve saves for you on start, listed as `local` | owner |
| Your other devices on the tailnet | Tailscale identity: the same user as the agent, on an untagged device other than the machine running the agent | owner |
| Everyone else on the private network, unless `--private-allow-incoming paired` | The tailnet's or relay's identity | authenticated |
| Messenger accounts | Linked with a pairing code | the pairing's role |
| People whose email matches `--allow-email` | A verified email: [Google sign-in](email-sign-in.md), a tailnet or relay login, or mail that passes DMARC | authenticated |
| Anyone else | A token from a pairing code | the pairing's role |
| [Webhook nodes](webhook-nodes.md) | Their node token | authenticated |
| Unlinked senders in direct chats, with `--public-allow-incoming anyone` | Their chat account | anonymous |
| Anyone on the public URL, with `--public-allow-incoming anyone` | Nothing | anonymous |

These settings are flags of `start` and `config set` ([reference](../reference/commands/config.md)). A running agent applies them at once, and every change prints who can call afterwards:

```sh
a2a-serve config set --private-allow-incoming paired           # on the private network, only you and your pairings
a2a-serve config set --allow-email '*@example.com'     # people with a matching verified email
a2a-serve config set --public-allow-incoming anyone            # anyone on the public URL or in a direct chat, as an anonymous caller
a2a-serve config unset allow-email '*@example.com'
```

- On the machine running the agent, the private URL doesn't make you an owner, because the sandbox reaches the tailnet through that machine too. Use the local URL there, or pair with `a2a-serve pair invite <name> --owner` to call the private URL as an owner. `pair join` saves the token in `~/.config/a2a-serve/credentials.json`, so don't mount that directory into the sandbox.
- Only owner pairings and linked owner chat accounts add owners. To make another account of yours an owner, pair it with `a2a-serve pair invite --owner`.
- A call that comes through another agent gets at most `authenticated`, even when the calling agent is yours.
- Each authenticated caller gets a separate container for conversation history, while sharing the workspace, `--mount` directories, and `--secret` environment variables ([Access Model](../advanced/access-model.md#agent-processes)).
- `--private-allow-incoming` and `--public-allow-incoming` take `everyone` and `anyone` as the same word: `--private-allow-incoming anyone` is `everyone`, and `--public-allow-incoming everyone` is `anyone`.
- [Anonymous callers](identities.md#anonymous-callers) start tasks in their own conversation and never answer approvals.
- A caller without a credential gets 401, with the steps to pair. A caller with a token the agent doesn't recognize, such as a mistyped, revoked or replaced one, gets 401 with the same steps. A known caller the agent doesn't accept gets 403, naming who they were seen as, such as `anna@example.com on the tailnet`. A refused call from another agent names that agent and gives the steps to pair agents: `pair invite` on this side and `contact add --code` on the caller's.

The [access model](../advanced/access-model.md) describes the model and its guarantees.

```sh
a2a-serve pair invite grok                        # authenticated: can talk to the agent
a2a-serve pair invite anna --owner                # owner: also sees and answers every task
a2a-serve pair invite anna                        # another code, for a second device
a2a-serve pair list                               # kind, role, linked messengers, last use
a2a-serve pair get anna                           # kind, role, tokens, linked accounts, pending codes
a2a-serve pair set anna --owner=false             # change the role
a2a-serve pair set laptop --kind device           # change the kind: person, device or agent
a2a-serve pair set anna --rotate                  # revoke her tokens and print a new code
a2a-serve pair rm grok                            # delete the pairing, its tokens, messenger links, schedules and tasks
```

A code works once, for 15 minutes. The caller redeems it with `a2a-serve pair join <agent-url> <code>`, or sends `join <code>` to the agent's bot. `a2a-serve pair list` also lists the agents you joined, and `--joined` lists only those from any directory, and `pair rm <agent-url>` forgets one. A second `pair join` to the same agent replaces your saved token, so it needs `--replace`.

- The first code redeemed sets the pairing's kind: `pair join` and `join <code>` make a person, and `contact add --code` makes an agent.
- A change of role applies to the next request of every token and messenger account of the pairing.
- `pair rm` also cancels the pairing's running tasks, stops its agent process, and deletes its tasks, approvals, conversations and agent home directory. Its conversations go with your messages in them. A new pairing with the same name starts with none of them. `pair rm <name> --dry-run` prints what it would remove and removes nothing.
- `pair rm` says when the person may still call through the private network or an `--allow-email` entry.
- The built-in `local` pairing is you. Its token works only on the local URL, and the link printed at start adds your messenger accounts to it. `pair rm local` revokes both, and the next start issues a new token. The names `local` and `owner` stand for you, so no other pairing can take them.

## Anonymous callers

With `--public-allow-incoming anyone`, anyone on the public URL, and anyone who writes to the agent's bot in a direct chat, can start tasks. The agent still ignores unlinked senders in group chats.

- Their conversations share an agent process, separate from yours and from each authenticated user's. With Docker it is a container with its own home directory, `state/home-anonymous`, so it can't read the history of other users' conversations. It works on the same workspace and receives the same `--secret` environment variables, and it isn't backed up.
- Their approvals go to the owner chat, or wait in the inspector. Only owners answer them. An approval that nobody answers in 10 minutes is rejected.
- Their conversations can't call other agents or use the schedules agent.
- The public URL and each messenger take at most 20 new anonymous tasks an hour, and run at most 2 at a time.
- A caller on the public URL without a token proves nothing about themselves. The task and conversation IDs that the server returns let them read and continue their own tasks. Anyone who has those IDs can do the same.
