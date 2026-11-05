# Configuration File

`config.yaml` holds what the flags set. Change it with `a2a-serve config set`,
`a2a-serve channel add` or `a2a-serve config edit`. Secret values are
references such as `secret:TELEGRAM_BOT_TOKEN`.

```yaml
name: coder
agent: opencode
command: [cloudcode, acp]        # optional: replaces the profile's command
sandbox: docker                  # or local
workspace: /home/u/src/app       # optional: default the directory holding .a2a
mounts: [/home/u/docs:/docs:ro]  # besides the workspace
model: openrouter/anthropic/claude-sonnet-4.5  # optional: the agent's own model id
secrets: [OPENROUTER_API_KEY]    # passed to the agent as environment variables
port: 46757                      # localhost port, chosen on the first run
network:
  private: tailscale
  public: tailscale
policy:
  privateAccess: paired          # default everyone
  publicAccess: paired           # or anyone
  allowEmails: [mom@example.com, "*@example.com"]
channels:
  telegram: {token: secret:CODER_BOT_TOKEN}
approvals:
  allow: [read, search]
  ownerChat: telegram:anna
mcp:
  disabled: [kubernetes]         # servers of .mcp.json the agent doesn't get
limits: {memory: 8g, cpus: "4", pids: 1024}
taskTTL: 30d                     # tasks idle this long are deleted
auth:
  googleClientID: 1234.apps.googleusercontent.com
  googleClientSecret: secret:GOOGLE_CLIENT_SECRET
backup:
  to: gs://my-bucket/agents/coder
  key: secret:BACKUP_KEY
  interval: 1h
  retention: 7d
```

## Keys

The table lists the key each flag sets. Keys are camelCase and nest at each
dot. Files with the snake_case keys of older versions still load, and
`a2a-serve doctor --fix` renames their keys.

<!-- generated:settings -->
| Flag | Key | Commands |
| --- | --- | --- |
| `--agent` | `agent` | `config set` |
| `--name` | `name` | `start <agent>`, `config set` |
| `--description` | `description` | `start <agent>`, `config set` |
| `--command` | `command` | `start <agent>`, `config set` |
| `--model` | `model` | `start <agent>`, `config set` |
| `--image` | `image` | `start <agent>`, `config set` |
| `--sandbox` | `sandbox` | `start <agent>`, `config set` |
| `--workspace` | `workspace` | `start <agent>`, `config set` |
| `--mount` | `mounts` | `start <agent>`, `config set` |
| `--secret` | `secrets` | `start <agent>`, `config set` |
| `--env` | `env` | `start <agent>`, `config set` |
| `--port` | `port` | `start <agent>`, `config set` |
| `--private` | `network.private` | `start <agent>`, `config set` |
| `--public` | `network.public` | `start <agent>`, `config set` |
| `--private-access` | `policy.privateAccess` | `start <agent>`, `config set` |
| `--public-access` | `policy.publicAccess` | `start <agent>`, `config set` |
| `--allow-email` | `policy.allowEmails` | `start <agent>`, `config set` |
| `--tailscale-control-url` | `network.tailscaleControlURL` | `config set` |
| `--tailscale-authkey` | the secret `TS_AUTHKEY` in `state/secrets` | `config set` |
| `--relay-token` | the secret `RELAY_TOKEN` in `state/secrets` | `config set` |
| `--token` | `channels.telegram.token` | `channel add telegram` |
| `--token` | `channels.discord.token` | `channel add discord` |
| `--email-address` | `channels.email.address` | `channel add email` |
| `--email-imap` | `channels.email.imap` | `channel add email` |
| `--email-smtp` | `channels.email.smtp` | `channel add email` |
| `--email-username` | `channels.email.username` | `channel add email` |
| `--email-password` | `channels.email.password` | `channel add email` |
| `--email-mailbox` | `channels.email.mailbox` | `channel add email` |
| `--email-trusted-authserv` | `channels.email.trustedAuthServ` | `channel add email` |
| `--email-oauth-token-url` | `channels.email.oauthTokenURL` | `channel add email` |
| `--email-oauth-client-id` | `channels.email.oauthClientID` | `channel add email` |
| `--email-oauth-client-secret` | `channels.email.oauthClientSecret` | `channel add email` |
| `--email-oauth-refresh-token` | `channels.email.oauthRefreshToken` | `channel add email` |
| `--allow-tools` | `approvals.allow` | `start <agent>`, `config set` |
| `--deny-tools` | `approvals.deny` | `start <agent>`, `config set` |
| `--disable-mcp` | `mcp.disabled` | `start <agent>`, `config set` |
| `--owner-chat` | `approvals.ownerChat` | `start <agent>`, `config set` |
| `--memory` | `limits.memory` | `config set` |
| `--cpus` | `limits.cpus` | `config set` |
| `--pids` | `limits.pids` | `config set` |
| `--google-client-id` | `auth.googleClientID` | `config set` |
| `--google-client-secret` | `auth.googleClientSecret` | `config set` |
| `--backup` | `backup.to` | `config set` |
| `--backup-key` | `backup.key` | `config set` |
| `--backup-interval` | `backup.interval` | `config set` |
| `--backup-retention` | `backup.retention` | `config set` |
| `--task-ttl` | `taskTTL` | `config set` |
<!-- /generated:settings -->
