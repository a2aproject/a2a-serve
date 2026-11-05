# Architecture

a2a-serve turns a coding agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com) (ACP), such as opencode or Claude Code, into an A2A agent. This page explains how it works. The [user guide](../index.md#user-guide) explains how to use it.

## Overview

- **One agent per directory.** `.a2a/config.yaml` holds the agent's settings, and `.a2a/state/` its database, secrets, network identity and home directory. One a2a-serve process serves one agent.
- **The agent runs in a sandbox.** a2a-serve starts the agent in a Docker container with the workspace mounted, and talks to it over ACP.
- **The server faces the world.** Callers reach the server over A2A on localhost, a private network, a public URL or a messenger. The server checks who they are and passes their messages to the agent.
- **People approve changes.** Every tool call that changes something pauses the task until the person who started it, or an owner, answers.
- **Calls to other agents go through the server.** The agent uses the `a2a` CLI. The server makes the call with its own credentials, so no token enters the sandbox.

## Components

| Component | What it does |
| --- | --- |
| Listeners | Accept requests on localhost, the private network (Tailscale or an Agent Mesh relay), the public URL (Funnel or a relay), the control socket and the agent's socket. |
| Auth | Turns each request into an identity, from a pairing token, a messenger link, a Tailscale or relay identity, a Google sign-in or a verified email sender, and gives it one role with the access policy. |
| A2A handler | Serves the agent card and the A2A protocol, and stores tasks in SQLite. |
| Messengers | Connect Telegram, Discord and email. Each chat or thread is one A2A conversation. |
| Executor | Keeps one ACP session per conversation, and turns ACP updates into A2A task updates and approval requests. Each authenticated user's conversations run in an agent process of their own, and anonymous conversations share one more. |
| Approvals | Decide who may answer a question, and deliver the answer to the task. |
| Sandbox | Runs the agent in Docker, or on the host with `--sandbox local`. |
| Agents proxy | Lets the agent call contacts and webhook nodes, and relays their approvals and late results. |
| Webhook nodes | Hold the tasks of agents that run only when woken, and wake them with signed POSTs. |
| Schedules | Run prompts at set times as new turns in a conversation. |
| Backups | Take encrypted snapshots of the agent directory. |
| Inspector | A web page where the owner sees conversations and answers approvals. |

## Pairing

Access starts with a one-time code from `a2a-serve pair invite`. Only someone with access to the agent directory can mint one, since the command goes through the control socket.

The caller redeems the code for a token. People run `a2a-serve pair join`, chat accounts send `join <code>` to the bot, other agents run `contact add --code`, and webhook nodes get a node token. The `a2a` CLI attaches saved tokens through a2a-serve's credential helper.

The user who starts the server is the built-in `local` pairing: it gets a token for the local URL, and the link printed at start links its messenger accounts. On a tailnet, your own devices are recognized by their network identity and need no code, and everyone else on the tailnet can call the agent.

## A turn

1. A message arrives over A2A or from a messenger. Auth decides who sent it.
2. The executor sends it to the agent's ACP session for that conversation. Sessions are stored, so conversations survive restarts.
3. When the agent wants to run a tool that changes something, the task pauses with an approval request.
4. The person who started the task answers, in the same place. The agent continues and finishes the task.

## Agents calling agents

The agent runs `a2a send` against a URL such as `http://a2a/k3/reviewer`. The request reaches the server through a socket only the agent can use. The handle `k3` stands for the conversation. The server accepts it only while the agent answers a message in that conversation, makes the call for the user who sent that message, and lets the conversation reach only the tasks it started. The agent's image carries the `a2a` version the server needs.

The server then:

- calls the other agent with its own token or network identity;
- relays the other agent's approval requests to the user;
- posts results that arrive later into the conversation as a new turn;
- stops chains of calls after 3 hops.

## Design notes

- **Why the `a2a` CLI.** It is already in the sandbox and covers the whole A2A protocol. ACP can pass only MCP servers to a session, and an MCP bridge added a tool surface for the agent to learn.
- **Why calls go through the server.** The private network runs inside the server process, contact tokens stay out of the sandbox, and the server can route approvals and late results to people.
- **What survives a restart.** Conversations and watched tasks are in `server.db`. A watched task stays there until the agent accepts its result as a turn, so a restart before then posts the result again. Open tasks fail on restart, including tasks that wait for an answer, so their questions close. Questions relayed from other agents stay open, because those tasks run at the other agent.
