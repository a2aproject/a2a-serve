# Configuration File

`config.yaml` holds what the flags set. Change it with `a2a-serve config set`,
`a2a-serve messenger add` or `a2a-serve config edit`. Secret values are
references such as `secret:TELEGRAM_BOT_TOKEN`. [Config](../guide/config.md#which-command-to-use) says which command to use.

```yaml
name: coder
description: Writes and reviews the Go services of this repository.
agent: opencode
command: [opencode, acp, --print-logs] # optional: replaces the profile's command
sandbox: docker                  # or local
workspace: /home/u/src/app       # optional: default the directory holding .a2a
mounts: [/home/u/docs:/docs:ro]  # besides the workspace
model: openrouter/~google/gemini-flash-latest  # optional: the agent's own model id
secrets: [OPENROUTER_API_KEY, GITHUB_TOKEN]  # environment variables of the agent, unless inject adds them
env: [LOG_LEVEL=debug]           # extra NAME=value environment variables
port: 46757                      # localhost port, chosen on the first run
network:
  private: tailscale
  public: tailscale
policy:
  privateAllowIncoming: paired   # default everyone
  publicAllowIncoming: paired    # or anyone
  allowEmails: [mom@example.com, "*@example.com"]
  privateAllowOutgoing: everyone # default contacts
  sandbox:                       # what agent containers get, see Sandbox rules
    - match: [email:*@example.com]
      storage: own
      secrets: [OPENROUTER_API_KEY]
messengers:
  telegram: {token: secret:CODER_BOT_TOKEN}
approvals:
  allow: [read, search, "tool:grafana.*", "command:kubectl get *"]
  deny: ["command:git push *"]
  ownerChat: telegram:anna
mcp:
  disabled: [kubernetes]         # servers of .mcp.json the agent doesn't get
limits: {memory: 8g, cpus: "4", pids: 1024}
egress:
  mode: internet                 # or allowlist, or unrestricted
  allow: [nas.lan:22, "*.corp.example.com", 192.168.1.0/24]
inject:                          # headers the server adds; the secrets stay out of the sandbox
  - model: openrouter            # --inject-model-key openrouter=OPENROUTER_API_KEY
    key: OPENROUTER_API_KEY
  - host: api.github.com         # --inject-header 'api.github.com=Authorization: Bearer $GITHUB_TOKEN'
    header: "Authorization: Bearer $GITHUB_TOKEN"
taskTTL: 30d                     # finished tasks idle this long are deleted
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

## Sandbox rules

`policy.sandbox` sets what the agent containers of chosen callers get of the workspace and of the agent's `secrets`. Callers that no rule matches get the defaults. The [sandbox guide](../guide/sandbox.md#what-each-callers-container-gets) gives the defaults, and explains the modes and the commands that edit the rules.

```yaml
secrets: [ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY]
policy:
  allowEmails: ["*@example.com"]
  sandbox:
    - match: [email:*@example.com]
      storage: own
      secrets: [ANTHROPIC_API_KEY]
    - match: [guests]
      secrets: [ANTHROPIC_API_KEY=secret:GUEST_ANTHROPIC_KEY]
```

| Key | Values |
| --- | --- |
| `match` | A list of matchers: `self`, `private`, `guests`, `email:<address>`, `email:*@<domain>` and `pairing:<name>`. `pairing:<name>` also matches a webhook node of that name. |
| `storage` | `shared`, `read-only`, `own` or `empty`, from the most access to the least. Without it, the storage of the next matching rule that sets one, or the default. |
| `secrets` | Names from the agent's `secrets`, `NAME=secret:OTHER`, which passes the stored secret `OTHER` to the agent as `NAME`, `all` or `none`. `all` and `none` go alone. Without it, the secrets of the next matching rule that sets them, or the default. |

A matcher may appear in one rule only, and config validation refuses duplicates. A rule may name only secrets that are in `secrets`.

### Which rule applies

- Owners, which are you and the owner pairings, share your agent container. Only the `self` rule applies to it.
- [Guests](../guide/identities.md#guests) share one container. Only the `guests` rule applies to it.
- Each [member](../guide/identities.md#roles) gets a container of their own, and the most specific matching rule applies. From most to least specific, the matchers are `pairing:<name>`, `email:<address>`, `email:*@<domain>` and `private`. Among rules of the same specificity, the first one listed wins. A part that rule leaves out, `storage` or `secrets`, comes from the next matching rule that sets it.

`guests` matches the callers that the [access policy](../advanced/access-model.md#policy) gives the guest role. `email:*@<domain>` matches only emails whose domain is proven, which are Google Workspace sign-ins and mail that passed DMARC, as `--allow-email` does.

### Commands

| Command | Rule it edits |
| --- | --- |
| `a2a-serve pair invite <name>` and `pair set <name>` with `--storage` and `--secrets` | The rule whose only matcher is `pairing:<name>`. A flag left out keeps that part of the rule. A pairing in a rule with other matchers moves to a rule of its own, which starts from that rule. |
| `a2a-serve pair rm <name>` | Removes the pairing from the rules. |
| `a2a-serve config set --guest-storage <mode> --guest-secrets <names>` | The rule whose only matcher is `guests`. `--guest-secrets` replaces its list. `config unset guest-storage` and `guest-secrets` reset it. |
| `a2a-serve config edit` | Any rule. |
| `a2a-serve config unset sandbox-rules` | Removes every rule except the rule of guests, including the rules of pairings and of `self`. |

Owner pairings refuse `--storage` and `--secrets`. The `self` rule sets what their container gets. The sandbox guide says [when a change applies](../guide/sandbox.md#when-a-change-applies).

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
| no flag (`card-skills` in `config unset`) | `card.skills` | `config edit` |
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
| `--guest-storage` | `policy.sandbox` | `start <agent>`, `config set` |
| `--guest-secrets` | `policy.sandbox` | `start <agent>`, `config set` |
| no flag (`sandbox-rules` in `config unset`) | `policy.sandbox` | `config edit` |
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
| `--egress` | `egress.mode` | `start <agent>`, `config set` |
| `--egress-allow` | `egress.allow` | `start <agent>`, `config set` |
| `--inject-header` | `inject` | `start <agent>`, `config set` |
| `--inject-model-key` | `inject` | `start <agent>`, `config set` |
| `--google-client-id` | `auth.googleClientID` | `config set` |
| `--google-client-secret` | `auth.googleClientSecret` | `config set` |
| `--backup` | `backup.to` | `config set` |
| `--backup-key` | `backup.key` | `config set` |
| `--backup-interval` | `backup.interval` | `config set` |
| `--backup-retention` | `backup.retention` | `config set` |
| `--voice` | `voice.service` | `config set` |
| `--voice-key` | `voice.key` | `config set` |
| `--voice-name` | `voice.name` | `config set` |
| `--task-ttl` | `taskTTL` | `config set` |
| `--contact-refresh` | `contactRefresh` | `config set` |
<!-- /generated:settings -->
