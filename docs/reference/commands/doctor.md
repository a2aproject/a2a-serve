# a2a-serve doctor

<!-- Generated from the command definitions; do not edit. -->

Check the a2a CLI and its config, the sandbox, and, inside an agent
directory or with --dir, the agent's config, secrets, server and service.
Every failure names the command which fixes it. Exits 1 if a check failed.

--fix enables command plugins and the credential helper in
~/.config/a2a-cli/config.yaml and makes credentials.json and .a2a/state
private. It never installs software and never touches a running agent.
--online also checks bot tokens and whether the agent image can be pulled.

```
a2a-serve doctor [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--fix` |  | Apply the local fixes and check again |
| `--online` |  | Also run checks which call external services |
