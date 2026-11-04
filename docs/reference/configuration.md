# Configuration file

`config.yaml` holds what the flags set; each key is changed with the flag of
the same name through `a2a-serve config set`. Secret values are references
such as `secret:TELEGRAM_BOT_TOKEN`.

```yaml
name: coder
agent: opencode
command: [cloudcode, acp]        # optional: replaces the profile's command
sandbox: docker                  # or local
workspace: /home/u/src/app       # optional: default the directory holding .a2a
mounts: [/home/u/docs:/docs:ro]  # besides the workspace
secrets: [OPENROUTER_API_KEY]    # passed to the agent as environment variables
port: 46757                      # localhost port, chosen on the first run
network:
  private: tailscale
  public: tailscale
policy:                          # --private-access, --public-access, --allow-email, --owner, --allow
  private_access: paired         # default everyone; paired on a relay that admits people by sign-in
  public_access: paired          # or anyone
  allow_emails: [mom@example.com, "*@example.com"]
  owner: [email:me@example.com]
  allow: [email:ben@example.org]
channels:
  telegram: {token: secret:CODER_BOT_TOKEN}
approvals:
  allow: [read, search]
  owner_chat: telegram:anna      # --owner-chat
limits: {memory: 8g, cpus: "4", pids: 1024}
task_ttl: 30d                    # --task-ttl: tasks idle this long are deleted
auth:                            # --google-client-id, --google-client-secret
  google_client_id: 1234.apps.googleusercontent.com
  google_client_secret: secret:GOOGLE_CLIENT_SECRET
backup:                          # --backup, --backup-key, --backup-interval, --backup-retention
  to: gs://my-bucket/agents/coder
  key: secret:BACKUP_KEY
  interval: 1h
  retention: 7d
```
