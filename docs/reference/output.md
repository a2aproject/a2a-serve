# Output and exit codes

Management commands take `-o json` or `-o jsonl`. List commands (`pair list`,
`channel list`, `contact list`, `schedule list`, `peers`, `backup list`,
`mailbox list`, `doctor`) print a JSON array of objects with `-o json`, and
one object per line with `-o jsonl`; each object has every column of the
table, and `pair list` and `peers` add a `kind`. `status -o json` is one
object with the same schema whether the agent runs or not: `running` is
always set, and the fields of the running server, such as `pid` and `local`,
only while it runs. Its `access` object says who may call: `privateAccess`
and `publicAccess` as in force, the members of the `owner`, `authenticated`
and `anonymous` roles, the counts of `pairings` and `ownerPairings`,
`others` who may call without a pairing, and the `summary` lines `status`
prints. `config set`, `config unset`, `pair invite`, `pair set` and `pair rm`
add the same `access` object after a change to who may call or to the
networks. Removals (`stop`, `channel rm`, `contact rm`, `pair rm`,
`schedule rm`, each also called `delete`) print
`{"removed": true, "kind": …, "name": …}`. `pair rm` adds `stillAllowed`, who
may still call the agent without the pairing, and `kept` for the `local`
pairing, which stays. `stop` prints
`{"name", "stopped", "serviceInstalled", "serviceRemoved"}`. `restart` prints the
same object as `status -o json`, and `channel add` prints the channel's `name` and
`account`. Progress lines of `restart` and `config set --restart` go to stderr with
`-o json`. Commands that
only print text, `start`, `logs`, `config edit`, `completion` and `help`, reject `-o`.

Commands that call the running agent take `--timeout`, the timeout of each
call (default 60s); `backup now` and `backup list` take it as the timeout of
the command (default 10m). `--version` prints what `version` prints.

Errors follow the error contract of the
[a2a CLI](https://github.com/a2aproject/a2a-cli) (`specification/SPEC.md`):
with `-o json` they are printed as `{"error": {"code", "message", "hint"}}`.

| Exit | Code | |
| --- | --- | --- |
| 0 | | Success |
| 1 | `A2ACLI_ERR_INTERNAL`, `A2ACLI_ERR_PROTOCOL` | Any other failure |
| 2 | `A2ACLI_ERR_USAGE` | Invalid arguments, flags or settings |
| 3 | `A2ACLI_ERR_IO` | An agent or service couldn't be reached |
| 4 | `A2ACLI_ERR_CARD_INVALID` | An agent card couldn't be used |
| 5 | `A2ACLI_ERR_TIMEOUT` | `--timeout` expired |
| 6 | `A2ASERVE_ERR_NOT_RUNNING` | The command needs the agent server, which isn't running: `a2a-serve start --detach` |
| 7 | `A2ASERVE_ERR_NO_AGENT` | No `.a2a` directory here, in a parent directory, or at `--dir` |
