# Messengers

People can talk to the agent through Telegram, Discord and email.

```sh
a2a-serve channel add telegram --token env:CODER_BOT_TOKEN
a2a-serve channel add discord --token env:CODER_DISCORD_TOKEN
a2a-serve channel add email --email-address coder@example.com \
  --email-imap imap.example.com:993 --email-smtp smtp.example.com:465 \
  --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com
a2a-serve channel list
a2a-serve channel rm discord
```

Each agent needs its own bot, because only one process can use a bot token. `channel add` prints a link that makes you the owner of the chat. Unknown senders get a hint on how to pair.

Each chat or thread is one conversation. `/new` starts a new one, and `/stop` cancels the running task. Messages, `/stop` and typed approvals are handled in the order they arrive. The agent tells you in the chat when an attachment can't be downloaded. Approvals show the request exactly as written, so that a link in a command can't hide part of it.

- **Telegram:** answers stream into one message, and approvals are buttons. In groups, the bot answers when mentioned or replied to.
- **Discord:** needs the Message Content intent. Each conversation gets a thread in server channels. If the agent can't start the thread, it says so and ignores the message. `/join`, `/new` and `/stop` are slash commands.
- **Email:** the agent accepts mail that passes DMARC or aligned DKIM at your trusted mail server (`--email-trusted-authserv`). It replies in the thread, with only questions and final results. Approve by replying `approve`, `always` or `deny`.
