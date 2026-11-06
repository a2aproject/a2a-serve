# a2a-serve logs

<!-- Generated from the command definitions; do not edit. -->

Print the agent's log.

```
a2a-serve logs [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `-f, --follow` |  | Keep printing new lines |
| `-n, --lines int` | `100` | Number of lines to print first; 0 prints all |
