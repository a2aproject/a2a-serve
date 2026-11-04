# What a2a-serve needs from the a2a CLI

- Command plugins: `a2a-<name>` on `PATH` runs as `a2a <name>`.
- `--unix-socket` (`A2ACLI_UNIX_SOCKET=<path>`): on Linux and with
  `--sandbox local`, the harness's `a2a` sends every request through the
  server's Unix socket.
- Credential helpers (`credential-helper`, the
  [Bazel credential helper protocol](https://github.com/EngFlow/credential-helper-spec)):
  `a2a-serve credential` returns the token `a2a-serve pair join` saved in
  `~/.config/a2a-serve/credentials.json` for the longest matching agent URL,
  or `$A2A_NODE_TOKEN` for its agent. `a2a-serve doctor --fix` lists it once
  under `credential-helper` in `~/.config/a2a-cli/config.yaml` as
  `{command: [a2a-serve, credential]}`. On Docker Desktop the harness gets its
  key from a helper script scoped to the bridge host
  (`A2ACLI_CREDENTIAL_HELPER=host.docker.internal=<path>`).
- `a2a skill` and `a2a version -o json`, which the server serves at
  `/skill.md` and `a2a-serve doctor` uses to check the installed version.
