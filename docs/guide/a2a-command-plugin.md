# Running as `a2a serve`

a2a-serve is a command plugin of the [a2a CLI](https://github.com/a2aproject/a2a-cli), so you can also run it as `a2a serve`.

The `a2a` CLI runs any `a2a-<name>` on `PATH` as `a2a <name>` when command plugins are enabled. With `a2a-serve` on `PATH`, `a2a serve start` runs `a2a-serve start`, and the same holds for every other command. This documentation writes every command as `a2a-serve <command>`.

Command plugins are off by default. `a2a-serve doctor --fix` turns them on. To turn them on by hand:

```bash
a2a plugin set-enabled true
```

`A2ACLI_PLUGINS_ENABLED=true` turns them on for one shell.

If `a2a serve` fails with `unknown command "serve" for "a2a"`, command plugins are off or `a2a-serve` is not on `PATH`.
