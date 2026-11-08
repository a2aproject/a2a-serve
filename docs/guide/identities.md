# Identities

The server recognizes each caller and gives it one of three roles: owner, member or guest.

## Roles

- **Owner**: you. Your local token, your own devices on the private network and your linked chat accounts are owners. Pairings made with `--owner` add more owners. Owners see every task and answer every approval.
- **Member**: a caller you let in. Other pairings, everyone else on the private network, people whose email matches `--allow-email` and webhook nodes are members. Members start tasks, and see and approve their own.
- **Guest**: a caller nobody vouched for. With `--public-allow-incoming anyone`, anyone on the public URL and strangers in direct chats are guests. A guest in a direct chat has a chat account, but nobody linked it to a pairing or allowed it. Guests start tasks in their own conversation and never answer approvals. See [Guests](#guests).

`a2a-serve status` says who can call right now:

```
  access   you: this machine and your tailnet devices
           everyone on the tailnet, emails matching *@example.com, 2 pairings
           the public URL needs a pairing or Google sign-in
```

| Caller | How it is recognized | Role |
| --- | --- | --- |
| You, on the machine running the agent | The token a2a-serve saves for you on start, listed as `local` | owner |
| Your devices on the tailnet | Tailscale identity: the same user as the agent, on an untagged device | owner |
| Everyone else on the private network, unless `--private-allow-incoming paired` | The tailnet's or relay's identity | member |
| Messenger accounts | Linked with a pairing code | the pairing's role |
| People whose email matches `--allow-email` | A verified email: [Google sign-in](email-sign-in.md), a tailnet or relay login, or mail that passes DMARC | member |
| Anyone else | A token from a pairing code | the pairing's role |
| [Webhook nodes](webhook-nodes.md) | Their node token | member |
| Unlinked senders in direct chats, with `--public-allow-incoming anyone` | Their chat account | guest |
| Anyone on the public URL, with `--public-allow-incoming anyone` | Nothing | guest |

These settings are flags of `start` and `config set` ([reference](../reference/commands/config.md)). A running agent applies them at once, and every change prints who can call afterwards:

```sh
a2a-serve config set --private-allow-incoming paired           # on the private network, only you and your pairings
a2a-serve config set --allow-email '*@example.com'     # people with a matching verified email
a2a-serve config set --public-allow-incoming anyone            # anyone on the public URL or in a direct chat, as a guest
a2a-serve config unset allow-email '*@example.com'
```

- Only owner pairings and linked owner chat accounts add owners. To make another account of yours an owner, pair it with `a2a-serve pair invite --owner`.
- A call that comes through another agent gets at most the member role, even when the calling agent is yours.
- Each member gets a separate container with a home of their own. What it gets of the workspace and the secrets follows the [sandbox defaults and rules](sandbox.md#what-each-callers-container-gets) ([Access Model](../advanced/access-model.md#agent-processes)).
- `--private-allow-incoming` and `--public-allow-incoming` take `everyone` and `anyone` as the same word: `--private-allow-incoming anyone` is `everyone`, and `--public-allow-incoming everyone` is `anyone`.
- A caller without a credential gets 401, with the steps to pair. A caller with a token the agent doesn't recognize, such as a mistyped, revoked or replaced one, gets 401 with the same steps. A known caller the agent doesn't accept gets 403, naming who they were seen as, such as `anna@example.com on the tailnet`. A refused call from another agent names that agent and gives the steps to pair agents: `pair invite` on this side and `contact add --code` on the caller's.

The [access model](../advanced/access-model.md) describes the model and its guarantees.

```sh
a2a-serve pair invite grok                        # member: can talk to the agent
a2a-serve pair invite anna --owner                # owner: also sees and answers every task
a2a-serve pair invite anna                        # another code, for a second device
a2a-serve pair list                               # kind, role, linked messengers, last use
a2a-serve pair get anna                           # kind, role, sandbox, tokens, linked accounts, pending codes
a2a-serve pair set anna --owner=false             # change the role
a2a-serve pair set laptop --kind device           # change the kind: person, device or agent
a2a-serve pair set anna --rotate                  # revoke her tokens and print a new code
a2a-serve pair invite ben --storage own --secrets ANTHROPIC_API_KEY  # what the pairing's container gets
a2a-serve pair rm grok                            # delete the pairing, its tokens, messenger links, schedules and tasks
```

A code works once, for 15 minutes. The caller redeems it with `a2a-serve pair join <agent-url> <code>`, or sends `join <code>` to the agent's bot. `a2a-serve pair list` also lists the agents you joined, and `--joined` lists only those from any directory, and `pair rm <agent-url>` forgets one. A second `pair join` to the same agent replaces your saved token, so it needs `--replace`.

- The first code redeemed sets the pairing's kind: `pair join` and `join <code>` make a person, and `contact add --code` makes an agent.
- A change of role applies to the next request of every token and messenger account of the pairing.
- `pair rm` also cancels the pairing's running tasks, stops its agent process, and deletes its tasks, approvals, conversations, sandbox rule, own copy of the workspace and agent home directory. Its conversations go with your messages in them. A new pairing with the same name starts with none of them. `pair rm <name> --dry-run` prints what it would remove and removes nothing.
- `pair rm` says when the person may still call through the private network or an `--allow-email` entry.
- The built-in `local` pairing is you. Its token works only on the local URL, and the link printed at start adds your messenger accounts to it. `pair rm local` revokes both, and the next start issues a new token. The names `local` and `owner` stand for you, so no other pairing can take them.

## Guests

With `--public-allow-incoming anyone`, anyone on the public URL, and anyone who writes to the agent's bot in a direct chat, can start tasks. The agent still ignores unlinked senders in group chats.

- Their conversations share an agent process, separate from yours and from each member's. With Docker it is a container with its own home directory, `state/home-anonymous`, so it can't read the history of other users' conversations. What it gets of the workspace and the secrets follows the [sandbox defaults](sandbox.md#what-each-callers-container-gets), and it isn't backed up.
- Their approvals go to the owner chat, or wait in the inspector. Only owners answer them. An approval that nobody answers in 10 minutes is rejected.
- Their conversations can't call other agents or use the schedules agent.
- The public URL and each messenger take at most 20 new guest tasks an hour, and run at most 2 at a time.
- A caller on the public URL without a token proves nothing about themselves. The task and conversation IDs that the server returns let them read and continue their own tasks. Anyone who has those IDs can do the same.
