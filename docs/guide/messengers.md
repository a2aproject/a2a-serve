# Messengers

People can talk to the agent through Telegram, Discord and email.

```sh
a2a-serve messenger add telegram --token env:CODER_BOT_TOKEN
a2a-serve messenger add discord --token env:CODER_DISCORD_TOKEN
a2a-serve messenger add email --email-address coder@example.com \
  --email-imap imap.example.com:993 --email-smtp smtp.example.com:465 \
  --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com
a2a-serve messenger list
a2a-serve messenger rm discord
```

Each agent needs its own bot, because only one process can use a bot token. `messenger add` prints a link that makes you the owner of the chat. Unknown senders get a hint on how to pair. `messenger list` shows a messenger as failing while it can't reach the chat service, for example after its bot token was revoked.

Each chat or thread is one conversation. `/new` starts a new one, `/stop` cancels the running task, and `/allowed` lists the tool calls that run without asking for you, with a button to remove each (see [Approvals](approvals.md#allowed-in-all-your-conversations)). Messages, `/stop` and typed approvals are handled in the order they arrive. Messages that you send while the agent works on a task reach it together in its next turn. The agent tells you in the chat when an attachment can't be downloaded. Approvals show the request exactly as written, so that a link in a command can't hide part of it. Bidi controls and zero-width spaces, which can make a command look like other text, show as codes such as `[U+202E]` below a warning line.

- **Telegram:** answers stream into one message, and approvals are buttons. In groups, the bot answers when mentioned or replied to. A message that arrives while the server stops can be lost.
- **Discord:** needs the Message Content intent. Each conversation gets a thread in server channels. If the agent can't start the thread, it says so and ignores the message. `/join`, `/new` and `/stop` are slash commands.
- **Email:** the agent reads mail that passes DMARC at your trusted mail server (`--email-trusted-authserv`). Without a DMARC result, or with `dmarc=none`, a DKIM signature of the sender's exact domain also counts. The agent drops other mail without a reply, `join` mails included. It replies in the thread, with only questions and final results. Approve by replying `approve`, `always` or `deny`. Email has no buttons, so it never offers to allow a call in all your conversations; remove what you allowed with `/allowed remove <call>`. The agent ignores quoted text, signatures and lines such as "Sent from my iPhone". When more text follows the answer, the agent asks for a plain answer.

DMARC and DKIM prove the domain of a sender. Anyone who can send authenticated mail for the domain counts as any of its addresses. This includes the domain's mail admins and services it lets send for it, such as a newsletter service.

Providers such as Microsoft 365 require OAuth sign-in for IMAP and SMTP. Pass a refresh token, the client ID and the provider's token URL with the `--email-oauth-*` flags of [`messenger add`](../reference/commands/messenger.md). The server refreshes the access tokens itself. When the provider replaces the refresh token, the server saves the new one with the agent's secrets.
