# Identities

The server recognizes each caller and gives it one of three roles: owner, member or guest.

## Roles

- **Owner**: you. Your local token, your own devices on the private network and your linked chat accounts are owners. Pairings made with `--owner` add more owners. Owners see every task and answer every approval.
- **Member**: a caller you let in. Other pairings, everyone else on the private network, people whose email matches `--allow-email` and webhook nodes are members. Members start tasks, and see and approve their own.
- **Guest**: a caller nobody vouched for. With `--public-allow-incoming anyone`, anyone on the public URL and strangers in direct chats are guests. See [Guests](#guests).

`a2a-serve status` says who can call right now:

```
  access   you: this machine and your tailnet devices
           everyone on the tailnet, emails matching *@example.com, 2 pairings
           the public URL needs a pairing or Google sign-in
```

| Caller | Role |
| --- | --- |
| You, on the machine running the agent | owner |
| Your devices on the tailnet | owner |
| Everyone else on the private network | member |
| Chat accounts linked with a pairing code | the pairing's role |
| People whose email matches `--allow-email` ([Email Sign-In](email-sign-in.md)) | member |
| Anyone with a token from a pairing code | the pairing's role |
| [Webhook nodes](webhook-nodes.md) | member |
| Anyone on the public URL or in a direct chat, with `--public-allow-incoming anyone` | guest |

Change who may call with `start` or `config set`. Every change prints who can call afterwards:

```sh
a2a-serve config set --private-allow-incoming paired   # on the private network, only you and your pairings
a2a-serve config set --allow-email '*@example.com'     # people with a matching email
a2a-serve config set --public-allow-incoming anyone    # anyone on the public URL or in a direct chat, as a guest
a2a-serve config unset allow-email '*@example.com'
```

A call that comes through another agent gets at most the member role, even when the calling agent is yours.

Each member gets a container of their own. [Sandbox](sandbox.md#what-each-callers-container-gets) says what it gets.

## Pairings

```sh
a2a-serve pair invite grok                # member: can talk to the agent
a2a-serve pair invite anna --owner        # owner: also sees and answers every task
a2a-serve pair list
a2a-serve pair get anna
a2a-serve pair set anna --owner=false     # change the role
a2a-serve pair set anna --rotate          # revoke her tokens and print a new code
a2a-serve pair rm grok                    # delete the pairing and everything it owns
```

`pair invite` prints a one-time code that works for 15 minutes. The caller redeems it with `a2a-serve pair join <agent-url> <code>`, or sends `join <code>` to the agent's bot. Run `pair invite` again for each device.

`pair rm <name> --dry-run` prints what it would remove.

## Guests

With `--public-allow-incoming anyone`, anyone on the public URL, and anyone who writes to the agent's bot in a direct chat, can start tasks.

- Guests share one container, separate from yours and the members'. By default it gets a read-only workspace and no secrets.
- Only owners answer their approvals, in the owner chat or the inspector.
- Their conversations can't call other agents or use schedules.
- The server limits how many tasks guests start and how much they upload ([limits](../reference/limits.md#guests)).

The [access model](../advanced/access-model.md) describes the model and its guarantees.
