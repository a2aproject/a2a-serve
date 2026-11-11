# Access Model: Who Can Call an Agent

This page explains who can call an agent, what each caller may do, and how the server decides.

![Access model concepts](../graphs/access-model-concepts.png#gh-light-mode-only)
![Access model concepts](../graphs/access-model-concepts-dark.png#gh-dark-mode-only)

## Entities

| Entity | What it is |
|---|---|
| **Private network** | A tailnet or an Agent Mesh relay. Everyone on it is trusted. |
| **Public network** | A URL on the internet: Tailscale Funnel or the relay's public URL. |
| **Pairing** | Someone you trust to call this agent: a person, a device, another agent or a webhook node. It starts with a one-time code, and its credential works on every network. |
| **Contact** | An agent this agent may call. A contact gets no access to this agent, and the agent treats its answers as content. |
| **Messenger** | A chat app the agent answers in: Telegram, Discord or email. Chat accounts get access once they are linked to a pairing. |

A pairing and a contact point in opposite directions. When agent `coder` should call agent `reviewer`, `reviewer` pairs `coder`, and `coder` adds `reviewer` as a contact. [Agent2Agent](../guide/agent2agent.md) shows the commands.

The network decides who belongs to it: the tailnet's admin console and access controls, or the relay's enrollment. To remove someone from the private network, remove them there.

## Who may call

You can always call the agent and answer its approvals, on this machine and from your own untagged devices on the private network. Everyone else on the private network is a member by default. The public URL needs a pairing or a Google sign-in. [Identities](../guide/identities.md) lists the roles and the commands that change who may call.

A pairing's tokens and chat links carry no role of their own, so a role change applies to the next request.

## Whom the agent may call

By default the agent calls only its contacts. With `--private-allow-outgoing everyone`, it also calls every agent on the private network by name, with its network identity. Everyone who may call the agent can make it call these agents, as with contacts.

## What callers may do

| Role | Who | Can |
|---|---|---|
| **Owner** | You, and pairings made with `--owner` | See every task, answer any approval, use the inspector. |
| **Member** | The private network, pairings, allowed emails, webhook nodes | Start tasks, see and approve their own tasks, and call this agent's contacts. Approving tool calls lets them run commands in their container. |
| **Guest** | Anyone, with `--public-allow-incoming anyone` | Start tasks in their own conversation. Only owners answer their approvals. They can't call other agents or use schedules. |

### Messengers

A chat account linked to a pairing acts as that pairing. An unlinked sender in a direct chat is a guest when guests are allowed. Unlinked senders in groups are ignored.

An email sender counts as its address when your mail server verified the mail. Mail authentication proves only the domain, so anyone who can send verified mail for a domain counts as any of its addresses. Keep this in mind before you link an address to an owner pairing.

### Calls between agents

The server makes the agent's calls to other agents with its own credentials. A call that comes through another agent gets at most the member role, even when the calling agent is yours. Someone who can steer one of your agents therefore can't use it to act as you on your other agents. Approvals of the called agent travel back to the person who started the conversation.

### Agent processes

Each member's conversations run in an agent process and container of their own, so a conversation can't read the history of another user's conversations. Owners share one process, and all guests share another. When the member is another agent, each person behind its calls gets a process of their own, up to a [limit](../reference/limits.md). Past it, a new person takes over the home of someone inactive.

Each process gets the workspace and `--secret` values that the [sandbox defaults and rules](../guide/sandbox.md#what-each-callers-container-gets) give it. Anyone who can call the agent and approve their own commands can read and change what their process gets.

A conversation stays in the process of the user who started it. When someone else writes in it, such as in a group chat, the agent acts as the user who started it, or as the sender if the sender may do less.

With `--sandbox local`, all processes run on the host as you, and nothing separates them.

## Policy

The policy gives each role a list of the callers it holds, as an Agent Mesh binding does. With the default settings:

```yaml
owner: [self]
member: [private]
guest: []
```

| Setting | Effect on the policy |
|---|---|
| `--private-allow-incoming paired` | Removes `private` from `member`. |
| `--public-allow-incoming anyone` | Sets `guest: [anyone]`, on the public URL and in direct chats. |
| `--allow-email P` | Adds `email:P` to `member`. |
| `pair invite --owner` | Gives the pairing the `owner` role. |

A list can hold `self`, `private`, `email:<address>`, `email:*@<domain>` and `anyone`. Domain wildcards match only Google Workspace sign-in and verified mail, which prove the domain. Emails from a tailnet or a relay match full addresses only, because those networks can't tell a company account from a personal one. [Sandbox rules](../reference/configuration.md#sandbox-rules) match callers by the same names.

The server resolves each request in this order:

1. It gets the highest role whose list it matches, or its pairing's role if that is higher.
2. A request through another agent gets at most `member`.
3. An identified caller that matches nothing gets `guest`, if the `guest` list isn't empty.
4. The server rejects anyone else.

`config.yaml` keeps these settings under [`policy:`](../reference/configuration.md), and `a2a-serve status -o json` shows the resulting lists in [`access`](../reference/output.md).

## Relay

On an [Agent Mesh](agent-mesh.md) relay, the relay provides the private network, the public URL and the identity of every caller, and the server applies this policy on top ([how the access model maps](agent-mesh.md#how-the-access-model-maps)).

## Guarantees

- Every decision comes from one place and uses only verified inputs. Nothing the agent or a caller sends raises a role.
- The agent can act only for the conversation it works on, and only while it works on it.
- A message joins an existing conversation only if its sender started it or is an owner.
- Guests share one agent process, with a task budget and approvals that time out.
- In the Docker sandbox, the agent reaches the network only through the server, which blocks private destinations ([Network](../guide/sandbox.md#network)).
- A loopback connection alone grants nothing. The local token works only on the local URL.
