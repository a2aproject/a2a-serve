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

Each agent needs its own bot. `messenger add` prints a link that makes you the owner of the chat. Unknown senders get a hint on how to pair.

Each chat or thread is one conversation. In the chat:

- `/new` starts a new conversation.
- `/stop` cancels the running task.
- `/allowed` lists the tool calls that run without asking you ([Approvals](approvals.md#allowed-in-all-your-conversations)).

## Telegram

Answers stream into one message, and approvals are buttons. In groups, the bot answers when mentioned or replied to.

## Discord

The bot needs the Message Content intent. Each conversation gets a thread in server channels. `/join`, `/new` and `/stop` are slash commands.

## Email

The agent replies in the thread, with questions and final results. Approve by replying `approve`, `always` or `deny`.

`--email-trusted-authserv` names your mail server. The agent reads only mail that this server verified as sent from the sender's domain, and drops the rest without a reply. Anyone who can send verified mail for a domain counts as any of its addresses.

For providers that need OAuth sign-in, such as Microsoft 365, use the `--email-oauth-*` flags of [`messenger add`](../reference/commands/messenger.md).
