# Architecture

a2a-serve turns a coding agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com) (ACP), such as opencode or Claude Code, into an A2A agent. This page explains the parts and how they fit. The [user guide](../index.md#user-guide) explains how to use it.

## Overview

- **One agent per directory.** `.a2a/config.yaml` holds the agent's settings, and `.a2a/state/` its database, secrets, network identity and home directory. One server process serves one agent. See the [agent directory](agent-directory.md).
- **The agent runs in a sandbox.** The server starts the agent in a Docker container with the workspace mounted, and talks to it over ACP.
- **The server faces the world.** Callers reach the server over A2A on localhost, a private network, a public URL or a messenger. The server checks who they are and passes their messages to the agent.
- **People approve changes.** Every tool call that changes something pauses the task until the person who started it, or an owner, answers.
- **Calls to other agents go through the server.** The agent uses the `a2a` CLI. The server makes the call with its own credentials, so no token enters the sandbox.

## Components

| Component | What it does |
| --- | --- |
| Listeners | Accept requests on localhost, the private network, the public URL and the local control socket. |
| Network providers | Join a network URL, such as an Agent Mesh relay or a SLIM node, as separate processes. See [network providers](#network-providers). |
| Auth | Turns each request into an identity and gives it one role. See the [access model](access-model.md). |
| A2A handler | Serves the agent card and the A2A protocol, and stores tasks in SQLite. |
| Messengers | Connect Telegram, Discord and email. Each chat or thread is one A2A conversation. |
| Executor | Keeps one ACP session per conversation and turns ACP updates into A2A task updates and approval requests. Each member's conversations run in an agent process of their own, and guests share one more. |
| Sandbox | Runs the agent in Docker, or on the host with `--sandbox local`. |
| Egress boundary | Gives each container its only network path, and decides each connection by its destination. It adds the secrets of inject rules to requests to their hosts. |
| Agents proxy | Lets the agent call contacts and webhook nodes, and relays their approvals and late results. |
| Webhook nodes | Hold the tasks of agents that run only when woken, and wake them with signed requests. |
| Approvals | Decide who may answer a question, and deliver the answer to the task. |
| Schedules, backups, inspector | Run prompts at set times, snapshot the agent directory, and show the owner conversations in a web page. |

## Pairing

Access starts with a one-time code from `a2a-serve pair invite`. The command goes through the control socket, so only someone with access to the agent directory can mint a code. The caller redeems the code for a token, and the `a2a` CLI attaches saved tokens through the server's credential helper. On a tailnet, your own devices are recognized by their network identity and need no code. See [Identities](../guide/identities.md).

## A turn

1. A message arrives over A2A or from a messenger. Auth decides who sent it.
2. The executor sends it to the agent's ACP session for that conversation. Sessions are stored, so conversations survive restarts.
3. When the agent wants to run a tool that changes something, the task pauses with an approval request.
4. The person who started the task answers, in the same place. The agent continues and finishes the task.

## Agents calling agents

The agent runs `a2a send` against a URL such as `http://a2a/k3/reviewer`. The request reaches the server through a socket only the agent can use. The server makes the call for the user whose message the agent is answering, with its own token or network identity.

The server also relays the other agent's approval requests to the user, posts results that arrive later into the conversation as a new turn, and stops chains of calls from looping. The [agent-to-agent guide](../guide/agent2agent.md) shows how users set this up.

## Network providers

Tailscale runs inside the server. Other networks run in a network provider: a separate program, in any language, that the server starts for the agent and controls over stdio. Agent Mesh is the first one, and a SLIM provider joins SLIM nodes.

- **The provider proves who calls.** It forwards each connection to the server unchanged, with a header that names the caller as the network proves it. The server terminates HTTP, applies its limits, and decides the caller's role with the same access model as on every network.
- **The server decides the rest.** It names the provider, decides who the owner is from the email the agent signed in with, and checks everything the provider returns.
- **Calls go out through the provider.** The server sends requests to other agents on the network through a tunnel the provider serves.
- **Providers stay out of reach.** The server runs only providers it ships, pins, or that the user installed by fingerprint, from a directory outside `PATH`. Their sockets live in a directory the sandbox can't reach, and each connection carries a token.

Each provider is its own Go module, so the core binary carries none of their dependencies, and a provider that needs cgo doesn't change how a2a-serve builds. The [protocol reference](../reference/network-providers.md) specifies the interface.

## Design notes

- **Why the `a2a` CLI.** It is already in the sandbox and covers the whole A2A protocol. ACP can pass only MCP servers to a session, and an MCP bridge adds a tool surface for the agent to learn.
- **Why calls go through the server.** The server reaches the private network, contact tokens stay out of the sandbox, and the server can route approvals and late results to people.
- **What survives a restart.** Conversations, schedules and the tasks the server watches for other agents are stored in the database. Tasks that were running fail on restart, and their open questions close.
