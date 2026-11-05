# a2a-serve channel

<!-- Generated from the command definitions; do not edit. -->

Manage the messengers the agent is reachable from

```
a2a-serve channel <command>
```

## a2a-serve channel add

Add a messenger, or replace its settings.

Secret values: env:VAR reads the value from the environment variable VAR; -
reads it from a prompt without echo, or from stdin when it isn't a terminal
(one flag per command); anything else is the value itself. In agent
settings, secret:NAME reuses a value stored in .a2a/state/secrets.

```
a2a-serve channel add <telegram|discord|email> [flags]
```

**Examples**

```bash
a2a-serve channel add telegram --token env:CODER_BOT_TOKEN
a2a-serve channel add email --email-address coder@example.com --email-imap imap.example.com:993 \
  --email-smtp smtp.example.com:465 --email-password env:MAIL_PASSWORD --email-trusted-authserv mx.example.com
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--email-address string` |  | Email channel: the agent's address |
| `--email-imap string` |  | Email channel: IMAP host:port, such as imap.example.com:993 |
| `--email-mailbox string` |  | Email channel: mailbox to watch (default INBOX) |
| `--email-oauth-client-id string` |  | Email channel: OAuth client ID |
| `--email-oauth-client-secret string` |  | Email channel: OAuth client secret, if the client has one (secret value: env:VAR, -, or the value) |
| `--email-oauth-refresh-token string` |  | Email channel: OAuth refresh token, which signs in with OAuth (XOAUTH2); the password is then unused (secret value: env:VAR, -, or the value) |
| `--email-oauth-token-url string` |  | Email channel: OAuth token endpoint, such as https://login.microsoftonline.com/common/oauth2/v2.0/token |
| `--email-password string` |  | Email channel: password (secret value: env:VAR, -, or the value) |
| `--email-smtp string` |  | Email channel: SMTP host:port, such as smtp.example.com:465 |
| `--email-trusted-authserv string` |  | Email channel: authserv-id of your mail server's Authentication-Results |
| `--email-username string` |  | Email channel: login (default: the address) |
| `--token string` |  | Bot token (secret value: env:VAR, -, or the value) |

## a2a-serve channel list

List the messengers, and whether they run

```
a2a-serve channel list
```

Aliases: `ls`

## a2a-serve channel rm

Remove a messenger

```
a2a-serve channel rm <telegram|discord|email>
```

Aliases: `delete`
