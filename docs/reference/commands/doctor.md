# a2a-serve doctor

<!-- Generated from the command definitions; do not edit. -->

Check the a2a CLI and its config, the sandbox, and, inside an agent
directory or with --dir, the agent's config, secrets, server and service.
In a relay directory, check only the relay's settings, state, agentmesh-one
and data. Every failure names the command that fixes it. Exits 1 if a
check failed.

The egress check tells whether the Docker sandbox's connections go through
the server. While the agent runs, it reports the server's own check, which
runs a container at each start and turns egress control off when the
container can't set up its network.

--fix enables command plugins and the credential helper in
~/.config/a2a-cli/config.yaml and makes credentials.json and .a2a/state
private. It never installs software and never touches a running agent.
--online also checks bot tokens, whether the agent image can be pulled,
that the speech service of --voice transcribes and speaks, and that the
relay answers at its URL with the Agent Mesh version this a2a-serve pins.

```
a2a-serve doctor [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--fix` |  | Apply the local fixes and check again |
| `--online` |  | Also run checks that call external services |
