# Limits

This page lists the limits, caps and timeouts of the server and the relay. You rarely hit them in normal use. When you do, the error or `a2a-serve status` says which one.

## Sandbox

| Limit | Value |
| --- | --- |
| Memory, CPUs and processes of a container | 4 GB, 2 CPUs and 512 processes. `--memory`, `--cpus` and `--pids` change them. |
| Member containers running at once | 10 |
| Homes for the people behind one calling agent | 32. Past that, a new person takes the home of the person whose last task is oldest. |
| Directories the server searches for other agents in a mount | 500,000 |
| Size of an `own` copy of the workspace | 1 GiB in 200,000 entries |
| `own` copies | 100. A copy of a caller without a pairing is deleted after 30 days without use. |
| A single message from the agent | 64 MiB |
| Task with no output from the agent | Fails after 10 minutes |

## Network

| Limit | Value |
| --- | --- |
| Open connections of a container | 256 |
| New connections of a container | 50 a second; more wait |
| Idle connection | Closed after 1 hour |

## Approvals

| Limit | Value |
| --- | --- |
| Unanswered approval in a member's task | Rejected after 4 hours |
| Unanswered approval in a guest's task | Rejected after 10 minutes |
| Other unanswered approvals | Rejected after 24 hours |
| Offer to allow a call in all your conversations | After "Always allow" in 3 conversations in a row |
| Calls a person allows in all their conversations | 50 |

## Agent calls

| Limit | Value |
| --- | --- |
| Wait for a reply when the agent calls another agent | 5 minutes. `--env A2ACLI_TIMEOUT=<duration>` changes it. |
| Hops in a chain of calls | 3 |
| Response from another agent | 32 MiB. Streams have no limit. |
| Result of an `a2a send --async` call | Over 16 KiB it arrives as the file `result.txt`. |
| Watch of an `--async` task | 7 days, or 24 hours without a change of state while it isn't waiting for an approval |
| Unfinished tasks of one caller | 8 |
| Refresh of contact and peer cards | Every hour. `--contact-refresh` changes it. |

## Schedules

| Limit | Value |
| --- | --- |
| `@every` interval | 1 minute or more |
| Schedules the agent creates for one person | 50, with runs at least 5 minutes apart. `a2a-serve schedule add` has neither limit. |

## Guests

| Limit | Value |
| --- | --- |
| New guest tasks | 20 an hour, for the public URL and for each messenger |
| Guest tasks running at once | 2, for the public URL and for each messenger |
| Attachments of guest tasks | 1 GiB in all. Past that, the oldest finished guest tasks show `[attachment unavailable]`. |
| Guest request bodies the public URL holds at once | 64 MiB. A request past that waits up to 30 seconds, then fails with 503. |

## Messengers

| Limit | Value |
| --- | --- |
| Attachment size | 20 MiB. The chat says when an attachment isn't passed to the agent. |
| Pairing code | Works once, for 15 minutes |
| Conversations `/conversations` lists for each person in a chat | 10 |

## Voice

| Limit | Value |
| --- | --- |
| Size of a recording | 25 MiB, and the attachment size of the messenger |
| Length of a recording | 5 minutes |
| Formats of a recording | Ogg Opus, MP3 and WAV. Owners can send other formats, which aren't measured. |
| Recordings in one message | 10 |
| Call of the speech service | 60 seconds |
| Speech calls of one person | 60 an hour, transcriptions and voice replies together. Guests share 60 an hour, for the public URL and for each messenger. Owners have no limit. |
| Spoken part of a reply | About 1,000 characters of prose, or about 4,000 characters of a `<speak>` block. A longer reply is cut, and the voice note ends with "The rest is in the text." |
| Voice replies of an email sender without a pairing | Turned off after 90 days without a message |

## Sign-in

| Limit | Value |
| --- | --- |
| Google sign-in of an agent | Expires after 90 days without use |
| Tokens of a removed email | Stop working at the next request |
| Inspector login link | Works once, for 5 minutes |
| Inspector session | 12 hours, or until the server restarts |

## Webhook nodes

| Limit | Value |
| --- | --- |
| Reply to a node task | Fails after 10 minutes without a reply |
| Wake of a webhook | 20 seconds; a slower wake fails and is retried |
| Clock skew of a wake signature | 5 minutes |
| Tasks waiting for a node | 20 for each caller, 100 in all. You and your agent aren't counted. |
| Pairings an agent advertises | 100 |

## Recipes

| Limit | Value |
| --- | --- |
| Size of a recipe | 10,000 files and 100 MB |

## Relay

| Limit | Value |
| --- | --- |
| Unused invite | Expires after 1 day. `--expires` changes it, up to 7 days. |
| Invited agent away from the relay | Needs a new invite after 30 days away |
| Database backups made by `relay upgrade` | The last 3 |
| Requests | [HTTP Routes](http-routes.md#relay-routes) |

## Logs

| Limit | Value |
| --- | --- |
| Server log | Rotated at 10 MB. The previous log is kept as `server.log.1`. |
