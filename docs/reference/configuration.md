# Configuration File

`config.yaml` holds what the flags set. Change it with `a2a-serve config set`,
`a2a-serve messenger add` or `a2a-serve config edit`. Secret values are
references such as `secret:TELEGRAM_BOT_TOKEN`. [Config](../guide/config.md#which-command-to-use) says which command to use.

```yaml
name: coder
description: Writes and reviews the Go services of this repository.
agent: opencode
command: [cloudcode, acp]        # optional: replaces the profile's command
sandbox: docker                  # or local
workspace: /home/u/src/app       # optional: default the directory holding .a2a
mounts: [/home/u/docs:/docs:ro]  # besides the workspace
model: openrouter/~google/gemini-flash-latest  # optional: the agent's own model id
secrets: [OPENROUTER_API_KEY]    # passed to the agent as environment variables
port: 46757                      # localhost port, chosen on the first run
network:
  private: tailscale
  public: tailscale
policy:
  privateAllowIncoming: paired   # default everyone
  publicAllowIncoming: paired    # or anyone
  allowEmails: [mom@example.com, "*@example.com"]
  privateAllowOutgoing: everyone # default contacts
messengers:
  telegram: {token: secret:CODER_BOT_TOKEN}
approvals:
  allow: [read, search, "tool:grafana.*", "command:kubectl get *"]
  deny: ["command:git push *"]
  ownerChat: telegram:anna
mcp:
  disabled: [kubernetes]         # servers of .mcp.json the agent doesn't get
limits: {memory: 8g, cpus: "4", pids: 1024}
taskTTL: 30d                     # tasks idle this long are deleted
contactRefresh: 1h               # how often the cards of contacts and peers are fetched
card:
  skills:                        # replace the card's default skill
    - id: release-notes
      name: Release notes
      description: Writes release notes from merged pull requests.
      tags: [git, docs]
auth:
  googleClientID: 1234.apps.googleusercontent.com
  googleClientSecret: secret:GOOGLE_CLIENT_SECRET
backup:
  to: gs://my-bucket/agents/coder
  key: secret:BACKUP_KEY
  interval: 1h
  retention: 7d
```

## Card skills

`card.skills` lists the skills of the agent card. Without it, the card lists one skill, "Coding agent". Each skill has an `id`, and may have a `name` (default: the id), a `description` and `tags`. IDs are unique and not empty. The card holds at most 20 skills, with descriptions of at most 1000 characters. `a2a-serve config unset card-skills <id>` removes a skill, and `a2a-serve card` prints the card.

## Keys

The table lists the key each flag sets. Keys are camelCase and nest at each
dot. Files with the keys of older versions still load: snake_case keys, and
`policy.privateAccess` and `policy.publicAccess`, which are
`policy.privateAllowIncoming` and `policy.publicAllowIncoming` now.
`a2a-serve doctor --fix` renames them.

<!-- generated:settings -->
| Flag | Key | Commands |
| --- | --- | --- |
| `--agent` | `agent` | `config set` |
| `--name` | `name` | `start <agent>`, `config set` |
| `--description` | `description` | `start <agent>`, `config set` |
|  | `card.skills` | `config edit` |
| `--command` | `command` | `start <agent>`, `config set` |
| `--model` | `model` | `start <agent>`, `config set` |
| `--image` | `image` | `start <agent>`, `config set` |
| `--sandbox` | `sandbox` | `start <agent>`, `config set` |
| `--workspace` | `workspace` | `start <agent>`, `config set` |
| `--mount` | `mounts` | `start <agent>`, `config set` |
| `--allow-broad-mount` | `allowBroadMounts` | `start <agent>`, `config set` |
| `--secret` | `secrets` | `start <agent>`, `config set` |
| `--env` | `env` | `start <agent>`, `config set` |
| `--port` | `port` | `start <agent>`, `config set` |
| `--private` | `network.private` | `start <agent>`, `config set` |
| `--public` | `network.public` | `start <agent>`, `config set` |
| `--private-allow-incoming` | `policy.privateAllowIncoming` | `start <agent>`, `config set` |
| `--public-allow-incoming` | `policy.publicAllowIncoming` | `start <agent>`, `config set` |
| `--private-allow-outgoing` | `policy.privateAllowOutgoing` | `start <agent>`, `config set` |
| `--allow-email` | `policy.allowEmails` | `start <agent>`, `config set` |
| `--tailscale-control-url` | `network.tailscaleControlURL` | `config set` |
| `--tailscale-authkey` | the secret `TS_AUTHKEY` in `state/secrets` | `config set` |
| `--relay-token` | the secret `RELAY_TOKEN` in `state/secrets` | `config set` |
| `--token` | `messengers.telegram.token` | `messenger add telegram` |
| `--token` | `messengers.discord.token` | `messenger add discord` |
| `--email-address` | `messengers.email.address` | `messenger add email` |
| `--email-imap` | `messengers.email.imap` | `messenger add email` |
| `--email-smtp` | `messengers.email.smtp` | `messenger add email` |
| `--email-username` | `messengers.email.username` | `messenger add email` |
| `--email-password` | `messengers.email.password` | `messenger add email` |
| `--email-mailbox` | `messengers.email.mailbox` | `messenger add email` |
| `--email-trusted-authserv` | `messengers.email.trustedAuthServ` | `messenger add email` |
| `--email-oauth-token-url` | `messengers.email.oauthTokenURL` | `messenger add email` |
| `--email-oauth-client-id` | `messengers.email.oauthClientID` | `messenger add email` |
| `--email-oauth-client-secret` | `messengers.email.oauthClientSecret` | `messenger add email` |
| `--email-oauth-refresh-token` | `messengers.email.oauthRefreshToken` | `messenger add email` |
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
| `--contact-refresh` | `contactRefresh` | `config set` |
<!-- /generated:settings -->
