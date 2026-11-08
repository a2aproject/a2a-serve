# Access Model: Who Can Call an Agent

> [!NOTE]
> The sandbox reaching the network only through the server is planned
> ([#17](https://github.com/a2aproject/a2a-serve/issues/17)).
> [Identities](../guide/identities.md) describes what
> works today.

An agent is reachable from your machine, a private network, a public URL and messengers. This page explains who can call it from each of those, and what each caller may do. The [data model reference](#data-model-reference) at the end describes the model underneath.

![Access model concepts](../graphs/access-model-concepts.png#gh-light-mode-only)
![Access model concepts](../graphs/access-model-concepts-dark.png#gh-dark-mode-only)

## Entities

| Entity | What it is | Commands and flags |
|---|---|---|
| **Agent** | Your coding agent in its sandbox | `start`, `stop`, `status`, `config` |
| **Private network** | Where your machines and team reach the agent: a tailnet or an Agent Mesh relay. Everyone on it is trusted | `--private tailscale\|<relay-url>\|none` |
| **Public network** | A URL on the internet: Tailscale Funnel or the relay's public URL | `--public tailscale\|<relay-url>\|none` |
| **Pairing** | Someone you trust to call this agent: a person, a device, another agent or a webhook node. Created with a one-time code; the credential works on every network | `pair invite\|join\|list\|get\|set\|rm` |
| **Contact** | An agent this agent may call. A contact gets nothing on this agent, and its answers are treated as content | `contact add\|list\|rm`; `--private-allow-outgoing everyone` |
| **Messenger** | A chat app the agent answers in: Telegram, Discord or email. Anyone can write to it, so chat accounts reach the agent once linked to a pairing | `messenger add\|list\|rm`; `join <code>` in the chat |

A pairing and a contact point in opposite directions. When agent `coder` should call agent `reviewer`, `reviewer` pairs `coder` (who may call me) and `coder` adds `reviewer` as a contact (whom I may call):

```bash
a2a-serve pair invite coder --dir ~/reviewer                     # prints a code
a2a-serve contact add reviewer --url <reviewer-url> --code <code> --dir ~/coder
```

## Who may call

| Flag | Values | Default |
|---|---|---|
| `--private-allow-incoming` | `everyone`: everyone on the private network. `paired`: only you and your pairings | `everyone` |
| `--public-allow-incoming` | `paired`: only you and your pairings. `anyone`: open to the internet | `paired` |
| `--allow-email <pattern>` | People who sign in with a matching email, such as `*@example.com`, on any network. Turns on Google sign-in at the public URL | none |
| `pair invite <name> --owner` | The pairing can also see every task and answer every approval | off |

You can always call the agent and answer its approvals: on this machine, and from your own devices on the private network that don't run an agent (see [the sandbox](security.md#the-sandbox)).

## Whom the agent may call

| Flag | Values | Default |
|---|---|---|
| `--private-allow-outgoing` | `contacts`: only its contacts. `everyone`: also every agent on the private network, by name | `contacts` |

With `everyone`, the agent calls agents on the private network without `contact add`, with its network identity. The agents line at the start of each conversation lists them. A contact by the same name wins. Everyone who may call the agent can make it call these agents, as with contacts.

The network decides who belongs to it: the tailnet's admin console and access controls, or the relay's enrollment. To remove someone from the private network, remove them there.

### Common tasks

| Task | Command |
|---|---|
| Share with a friend | `a2a-serve pair invite anna`; Anna runs `a2a-serve pair join <agent-url> <code>`, or sends `join <code>` to the bot |
| Call from my phone | On the tailnet: nothing. Elsewhere: `a2a-serve pair invite phone --owner` |
| Let another agent call this one | `pair invite` here, `contact add --code` there (see above) |
| Team on a relay | `--private <relay-url>`: everyone the relay admits can call |
| Let agents on the private network call each other | `--private-allow-outgoing everyone` on each agent |
| Only company emails | `--allow-email '*@example.com'` |
| Close the private network | `--private-allow-incoming paired` |
| Public bot | `--public tailscale --public-allow-incoming anyone` |
| Revoke a pairing | `a2a-serve pair rm anna` |

### What you see

`a2a-serve status` says who can call right now:

```
coder (opencode) running for 2h, pid 41233
  local    http://127.0.0.1:46757
  private  https://coder.tail1234.ts.net
  public   https://coder.tail1234.ts.net
  access   you: this machine and your other tailnet devices that don't run an agent
           everyone on the tailnet, emails matching *@example.com, 2 pairings
           the public URL needs a pairing or Google sign-in
```

- Every change to access prints who can call afterwards.
- `pair rm` says when the person can still call through the private network or an email rule.
- A caller without a credential gets 401, with the steps to pair.
- A known caller the agent doesn't allow gets 403, naming who they were seen as.

## What callers may do

| Caller | Can |
|---|---|
| **You**, and pairings made with `--owner` | See every task, answer any approval, use the inspector |
| **Everyone else who is allowed**: the private network, pairings, allowed emails | Start tasks, see and approve their own tasks, allow tool calls in their own tasks without asking, and let their conversations call this agent's contacts. Approving tool calls lets them run commands in the shared workspace and read the agent's `--secret` environment variables |
| **Anyone**, with `--public-allow-incoming anyone` | Start tasks in their own conversation. Their approvals go to your owner chat, and are rejected when nobody answers in time. Their conversations can't call other agents |

### Messengers

| Sender | Treated as |
|---|---|
| Chat account linked to a pairing (`join <code>`, or the link printed at start) | the pairing. The link printed at start links you |
| Email sender whose address passes DMARC at your trusted mail server (`--email-trusted-authserv`) | that email address, so `--allow-email anna@example.com` lets Anna email the agent without linking. DMARC proves the domain only, so anyone who can send authenticated mail for it counts as any of its addresses |
| Unlinked sender in a direct chat | anyone, with `--public-allow-incoming anyone`; otherwise rejected with a hint on how to pair |
| Unlinked sender in a group | ignored |

Each chat is one conversation. Approvals appear in the chat where the task started, and only the person who started the task, or you, can answer them. Approvals of anonymous senders and of tasks nobody watches go to your owner chat.

### Calls between agents

The agent calls other agents through its server, which holds the credentials. The receiving agent gives a call that came through another agent at most the rights of "everyone else who is allowed", even when the calling agent is yours. The task runs, and its approvals travel back to the person who started the conversation. Someone who can steer one of your agents therefore can't use it to act as you on your other agents.

### Agent processes

Each authenticated user's conversations run in an agent process of their own. Your conversations run in the owner's process, and all anonymous callers share one more. In Docker each process has its own container and home directory, so a conversation can't read the history of another user's conversations. At most 10 processes of authenticated users run at once. When all of them work on tasks or wait for approvals, a user without a running process gets an error for a new task and can send it again later. All processes share the workspace and the agent's `--secret` environment variables, so anyone who can call the agent and approve their own commands can change the workspace and read its files and the agent's secrets.

A conversation stays in the process of the user who started it. When someone else sends a message there, the agent calls other agents and the schedules agent as the user who started it, or as the sender if the sender may do less.

With `--sandbox local`, the processes run on the host as you, and nothing separates them. The server warns at start when others may call such an agent.

## Data model reference

### Identity

Every request yields one identity: verified claims, and where each came from.

| Field | Source |
|---|---|
| network | local, private, public, chat or the agent's socket, and the provider |
| email | tailnet login of an untagged user; relay principal, when it is a verified email; Google sign-in; email sender with DMARC pass |
| node | tailnet `StableID` or mesh peer ID; tagged tailnet nodes have only this |
| pairing | the pairing whose token or chat link was used |
| chat account | `telegram:<user id>`, `discord:<user id>`, or the email address |
| self | Tailscale: the same user on an untagged device that doesn't run an agent. Relay: a verified email equal to the one saved when this agent signed in. Never on the public URL |
| via agent | the request came from another server, with a hop count of 1 or more |

### Roles

| Role | Rights |
|---|---|
| `owner` | Sees every task, answers any approval, uses the inspector |
| `authenticated` | Starts tasks, sees and approves its own tasks, and allows tool calls in them without asking; its conversations may call contacts |
| `anonymous` | Starts tasks in its own conversation; never answers approvals, calls agents or uses the schedules agent |

### Policy

The policy gives each role a list of members, as an Agent Mesh binding does. The `owner` role holds only `self`; owner pairings add other owners.

```yaml
policy:
  owner: [self]
  authenticated: [private]
  anonymous: []
```

| Flag | Policy |
|---|---|
| defaults | `owner: [self]`, `authenticated: [private]`; the built-in `local` pairing is owner |
| `--private-allow-incoming paired` | removes `private` from `authenticated` |
| `--public-allow-incoming anyone` | `anonymous: [anyone]`, on the public URL and in direct chats |
| `--allow-email P` | adds `email:P` to `authenticated` |
| `pair invite --owner` | the pairing's role is `owner` |

Members are `self`, `private`, `email:<address>`, `email:*@<domain>` and `anyone`. Domain wildcards match Google Workspace sign-in and mail that passes DMARC, which prove the domain. Emails from the tailnet or a relay match full addresses only, because those networks can't tell a company account from a personal account at the same domain.

`config.yaml` keeps the flags under [`policy:`](../reference/configuration.md), and `a2a-serve status -o json` shows the resulting bindings in [`access`](../reference/output.md).

A request is resolved in this order:

1. It gets the highest role it matches, across the bindings and its pairing.
2. A request via another agent gets at most `authenticated`.
3. An identified caller that matches nothing gets `anonymous`, if `anonymous` has members.
4. Anyone else is rejected.

### Pairings

A pairing is a record with a name, a kind (person, device, agent or webhook) and a role. Tokens and chat links point to it and carry no role of their own, so a role change applies to the next request. A webhook pairing's token opens the node's mailbox and lets the node call the agent as `authenticated`.

The first code redeemed sets the kind: `pair join` and `join <code>` in a chat make a person, `contact add --code` makes an agent, and `pair invite --webhook` makes a webhook. `pair set --kind` changes it.

The built-in `local` pairing is you. Its token works only on the local listener, and chat accounts linked with the code printed at start are its links. Google sign-in tokens carry the email and belong to no pairing.

## Relay

On an [Agent Mesh](agent-mesh.md) relay, the relay provides the private network, the public URL and the identity of every caller, and a2a-serve applies this policy on top. The Agent Mesh page maps each entity onto the relay.

## Guarantees

- Every decision comes from one place, and only from verified inputs. Nothing the agent or a caller sends raises a role.
- A call that comes through another agent gets at most `authenticated`.
- An agents-URL handle works only for its conversation, while one of its turns runs, and only for the tasks its conversation created.
- The sandbox reaches the network only through the server, which refuses private, tailnet, link-local and host addresses.
- A message joins an existing conversation only if its sender owns it or is an owner.
- Each authenticated user has their own agent process. Callers allowed by `--public-allow-incoming anyone` share one agent process, with a budget per listener and approvals that time out.
- Requests that change state need a JSON content type or the `A2A-Version` header, and cross-site requests are refused. The question and inspector forms rely on the cross-site check and the inspector's CSRF token.
- The local credential works only on the local listener. Loopback alone is never trusted.
