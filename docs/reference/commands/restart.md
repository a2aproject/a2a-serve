# a2a-serve restart

<!-- Generated from the command definitions; do not edit. -->

Stop the agent and start it again in the same mode, detached or as a
user service, to apply settings which wait for a restart. The settings are
checked first, so an agent which couldn't start again isn't stopped. An
agent running in the foreground of a terminal is restarted there: press
Ctrl-C and run `a2a-serve start` again.

```
a2a-serve restart [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--quiet` |  | Print less on the terminal: keep the agent image pull and build output in the log |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |
