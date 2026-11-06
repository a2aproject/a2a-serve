# a2a-serve stop

<!-- Generated from the command definitions; do not edit. -->

Stop the agent.

```
a2a-serve stop [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |
| `--uninstall` |  | Also remove the launchd or systemd service |
