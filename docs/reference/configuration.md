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

`policy.sandbox` narrows what the agent containers of chosen callers get of the workspace and of the agent's `secrets`. A rule never gives more than the caller's default. The [sandbox guide](../guide/sandbox.md#what-each-callers-container-gets) gives the defaults, and explains the modes and the commands that edit the rules.

```yaml
secrets: [ANTHROPIC_API_KEY, GITHUB_TOKEN, GUEST_ANTHROPIC_KEY]
policy:
  allowEmails: ["*@example.com"]
  sandbox:
    - match: [email:*@example.com]
      storage: own
      secrets: [ANTHROPIC_API_KEY]
    - match: [anyone]
      secrets: [ANTHROPIC_API_KEY=secret:GUEST_ANTHROPIC_KEY]
```

| Key | Values |
| --- | --- |
| `match` | A list of matchers: `self`, `private`, `anyone`, `email:<address>`, `email:*@<domain>` and `pairing:<name>`. `pairing:<name>` also matches a webhook node of that name. |
| `storage` | `shared`, `read-only`, `own` or `empty`, from the most access to the least. It applies when it gives less than the default. Without it, the default. |
| `secrets` | Names from the agent's `secrets`, `NAME=secret:OTHER`, which passes the stored secret `OTHER` to the agent as `NAME`, `all` or `none`. `all` and `none` go alone. A name applies when the default gives its stored secret. Without it, the default. |

A matcher may appear in one rule only, and config validation refuses duplicates. A rule may name only secrets that are in `secrets`.

### Which rule applies

- Owners, which are you and the owner pairings, share your agent container. Only the `self` rule applies to it.
- Anonymous callers share one container. Only the `anyone` rule applies to it, and it can only narrow their read-only workspace to an empty one.
- Every other caller gets a container of their own, and the most specific matching rule applies. From most to least specific, the matchers are `pairing:<name>`, `email:<address>`, `email:*@<domain>`, `private` and `anyone`. Among rules of the same specificity, the first one listed wins. A part that rule leaves out, `storage` or `secrets`, comes from the next matching rule that sets it.

In sandbox rules, `anyone` matches every caller who isn't an owner. In the [access policy](../advanced/access-model.md#policy), `anyone` means the public URL and direct chats. `email:*@<domain>` matches only emails whose domain is proven, which are Google Workspace sign-ins and mail that passed DMARC, as `--allow-email` does.

### Commands

| Command | Rule it edits |
| --- | --- |
| `a2a-serve pair invite <name>` and `pair set <name>` with `--storage` and `--secrets` | The rule whose only matcher is `pairing:<name>`. A flag left out keeps that part of the rule. A pairing in a rule with other matchers moves to a rule of its own, which starts from that rule. |
| `a2a-serve pair rm <name>` | Removes the pairing from the rules. |
| `a2a-serve config set --caller-storage <mode> --caller-secrets <names>` | The rule whose only matcher is `anyone`: the rule of other callers. `--caller-secrets` replaces its list. `config unset caller-storage` and `caller-secrets` reset it. |
| `a2a-serve config edit` | Any rule. |
| `a2a-serve config unset sandbox-rules` | Removes every rule except the `anyone` rule, including the rules of pairings and of `self`. |

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
| `--caller-storage` | `policy.sandbox` | `start <agent>`, `config set` |
| `--caller-secrets` | `policy.sandbox` | `start <agent>`, `config set` |
|  | `policy.sandbox` | `config edit` |
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
