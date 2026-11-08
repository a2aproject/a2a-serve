# Output and Exit Codes

Management commands take `-o json` or `-o jsonl`, and `config show` also takes `-o yaml`. List commands (`pair list`,
`messenger list`, `contact list`, `schedule list`, `peers`, `backup list`,
`mailbox list`, `doctor`) print a JSON array of objects with `-o json`, and
one object per line with `-o jsonl`. [Text columns and JSON fields](#text-columns-and-json-fields)
lists the field behind each column. `pair list` gives each entry
a `side`: `invited` for a pairing of the agent, `joined` for an agent you joined. `status -o json` is one
object with the same schema whether the agent runs or not: `running` is
always set, and the fields of the running server, such as `pid` and `local`,
only while it runs. Its `access` object says who may call: `privateAllowIncoming`
and `publicAllowIncoming` as in force, the members of the `owner`, `authenticated`
and `anonymous` roles, the counts of `pairings` and `ownerPairings`,
`others` who may call without a pairing, and the `summary` lines `status`
prints. With sandbox rules or guests, it adds the `sandbox` lines
of what each kind of caller's agent container gets, `sandboxNotes` such as
callers without a model key, and `anonymousNote` about guests. `pair get` and `pair list`
give each pairing a `sandbox` object: its `storage`, its `secrets`, a
`summary`, and a `warning` when its agent can't reach its model. `pair
invite` adds it when rules change what the pairing gets, and `pair set`
adds `hints`. `status` and `stop` add `configError` when `.a2a/config.yaml` is
invalid or missing. `config set`, `config unset`, `pair invite`, `pair set` and `pair rm`
add the same `access` object after a change to who may call or to the
networks. Removals (`messenger rm`, `contact rm`, `pair rm`,
`schedule rm`, each also called `delete`) print
`{"removed": true, "kind": …, "name": …}`. `pair rm` adds `stillAllowed`, who
may still call the agent without the pairing, and `kept` for the `local`
pairing, which stays. `pair rm --dry-run` removes nothing and prints the same
`kind` and `name` with `"dryRun": true` and the counts of what it would remove. `pair rm --email` prints the kind `google` and adds
`tokens`, the number of sign-in tokens it revoked. `stop` prints
`{"name", "stopped", "serviceInstalled", "serviceRemoved"}`, where `stopped` is
false if the agent wasn't running and `serviceInstalled` is true if it starts
again at login. `restart` prints the object of `status -o json` for the running
agent, without `pending`. `messenger add` prints the messenger's `name` and
`account`, and `ownerLink` with the `hint`, `code` and `expires` of the link
that makes you its owner, until you link an account. Progress lines of `restart`, `config set --restart` and `config unset --restart` go to stderr with
`-o json`. `config show` prints an invalid config with the reason in `invalid`, and exits 1. Commands that
only print text, `start`, `logs`, `config edit`, `completion` and `help`, reject `-o`.

Commands that call the running agent take `--timeout`, the timeout of each
call (default 60s); the `mailbox` commands take their own, with a default of 30s. The `backup` commands take `--deadline`, the time limit of
the whole command (default 10m). `--version` prints what `version` prints.

Errors follow the error contract of the
[a2a CLI](https://github.com/a2aproject/a2a-cli) (`specification/SPEC.md`):
with `-o json` they are printed as `{"error": {"code", "message", "hint"}}`.
A failure that the running agent reports keeps its code.

| Exit | Code | |
| --- | --- | --- |
| 0 | | Success |
| 1 | `A2ACLI_ERR_INTERNAL`, `A2ACLI_ERR_PROTOCOL` | Any other failure |
| 2 | `A2ACLI_ERR_USAGE` | Invalid arguments, flags, flag combinations or settings, such as an agent URL without a scheme and host; a missing setting such as the backup destination; or a name or file that doesn't exist, such as `pair rm` of an unknown pairing or of an agent you haven't joined, `mailbox reply --artifact` of a missing file, or `logs` before the first log |
| 3 | `A2ACLI_ERR_IO` | An agent or service couldn't be reached |
| 5 | `A2ACLI_ERR_TIMEOUT` | `--timeout` or `--deadline` expired |
| 6 | `A2ASERVE_ERR_NOT_RUNNING` | The command needs the server, which isn't running: `a2a-serve start --detach`. `status --url local` before the first start gives it too, since the first start picks the port |
| 7 | `A2ASERVE_ERR_NO_AGENT` | No `.a2a` directory here, in a parent directory, or at `--dir`, including `start` without an agent outside a terminal |
| 8 | `A2ASERVE_ERR_BUSY` | The server is starting or stopping; run the command again in a moment |

## Text columns and JSON fields

The text output of a list command shows some fields under another name or in a
shorter form. The table gives the JSON field behind each column. Fields that
the text output doesn't show are listed after it.

| Command | Column | JSON field |
| --- | --- | --- |
| `pair list` (invited) | `NAME`, `KIND`, `ROLE` | `name`, `kind`, `role` |
| | `LINKED` | `accounts`, objects with `messenger` and `account`; the text lists the messengers |
| | `LAST USED` | `lastUsed`; the text shows `never`, `not paired`, `code pending` or `no credentials` when it is unset |
| `pair list` (joined) | `URL`, `AGENT`, `JOINED` | `url`, `agent`, `joined` |
| | `AS` | `principal` |
| `contact list` | `NAME`, `URL` | `name`, `url` |
| | `ROUTE` | `via` is `private` or `direct`; the text shows `private network` or `direct connection` |
| | `AUTH` | `hasToken`; the text shows `token` or `network identity` |
| `schedule list` | `NAME`, `CHAT`, `NEXT` | `name`, `chat`, `nextRun` |
| | `WHEN` | `cron` or `at`, with `tz`; the text shows `once` for `at` |
| | `AS` | `user`, the principal the runs are for; the text shows its display name |
| | `LAST` | `lastRun`, with `lastError` |
| | `PROMPT` | `prompt`, in full; the text cuts it |
| `messenger list` | `MESSENGER`, `ACCOUNT` | `name`, `account` |
| | `STATE` | `state`, with `error`; the text joins them as `state: error` |
| `peers` | `NAME`, `URL`, `DESCRIPTION` | `name`, `url`, `description` |
| | `STATE` | `online`; the text shows `online` or `offline` |
| `backup list` | `SNAPSHOT`, `TAKEN` | `name`, `taken` |
| `mailbox list` | `TIME`, `KIND`, `TASK`, `TEXT` | `envelope.createdAt`, `envelope.kind`, `envelope.taskId`, `envelope.text` |
| | `SENDER` | `envelope.sender`, an object with `kind` and `name` |
| | `DELIVERED`, `OPEN` | `delivered`, `open` |

The JSON of `pair list` adds `kind` (`caller` or `node`) to joined entries,
and `principal`, `created`, `tokens`, `node` and `pendingCodes` to invited
ones. `pendingCodes` holds the expiry times of the codes that haven't been
used; codes are stored hashed, so they can't be shown again. `schedule list`
adds `thread` and `createdBy`, `peers` adds `kind` (`agent` or `advertised`),
`backup list` adds `agent`, and `mailbox list` adds `attempts`, `lastError`
and the rest of the `envelope`. `doctor -o json` lists each check with `id`, `status`, `message` and `fix`.
