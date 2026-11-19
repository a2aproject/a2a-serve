# Limits

This page lists the limits, caps and timeouts of the server, the relay and `a2a-run`. You rarely hit them in normal use. When you do, the error or `a2a-serve status` says which one.

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

## Network providers

| Limit | Value |
| --- | --- |
| A message on the provider's stdin or stdout | 1 MiB |
| Answer to `describe` or `member` | 30 seconds; `listen` 2 minutes, `peers` 15 seconds, `join` no limit |
| Health check of a running provider | `describe` every 10 seconds, answered within 5 seconds; reported as not answering after 3 missed answers in a row |
| Start of a streamed response from an agent on the network | 1 minute, then the call fails as unreachable |
| Header of a connection from the provider | 4096 bytes, sent within 10 seconds |
| Connections from the provider waiting for their header | 256 |
| Exit after the server closes the provider's stdin | 10 seconds, then the server kills it |
| Release archive of a provider | 512 MiB |

## Messages

| Limit | Value |
| --- | --- |
| Parts of a message | 1000 |
| Text and data of a message | 1 MiB in all. A data part counts with its JSON. |
| Metadata of a message and its parts | 64 KiB of JSON in all |
| `extensions` of a message | 32, each at most 1 KiB |
| `referenceTaskIds` of a message | 32, each at most 1 KiB |
| Files of a message | The largest request body ([HTTP Routes](http-routes.md#request-limits)) |

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

## Requests

| Limit | Value |
| --- | --- |
| Open requests and streams of one member | 32 on each URL of the agent. Past that, a request fails with 429. You have no limit. |
| Writing a response | 1 minute without progress, and 10 minutes in all. A stream stays open while its client reads it. |
| Downloads of `a2a` and `a2a-serve` from the agent | 8 at once on each URL of the agent, and 2 from one client. Past that, a download waits up to 30 seconds for its turn, then fails with 503. |
| Writing a download | 1 minute without progress, and 10 minutes in all |

## Schedules

| Limit | Value |
| --- | --- |
| `@every` interval | 1 minute or more |
| Schedules the agent creates for one person | 50, with runs at least 5 minutes apart. `a2a-serve schedule add` has neither limit. |

## Backups

| Limit | Value |
| --- | --- |
| Partial snapshot an interrupted backup left in a directory | Removed by the first backup after 1 day |

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
| Size of a recording | 25 MiB, 14 MiB with `gemini`, and the attachment size of the messenger |
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

## a2a-run

| Limit | Value |
| --- | --- |
| Agent processes running at once | 4 for the A2A contexts. `--max-processes` changes it, up to 64. One more process runs for the life of `a2a-run`, started to check that the agent starts. |
| Idle agent process | Stops after 10 minutes |
| Tasks running at once | 64 |
| Question with no answer | The task fails after 4 hours |
| Memory, CPUs and processes of a container | 4 GB, 2 CPUs and 512 processes |
| Length of a context ID | 256 characters |
| Tasks and agent sessions of a context | Forgotten 1 hour after the context's last change, once all its tasks ended |
| Contexts kept | 1000. Past that, or past 256 MB of tasks, `a2a-run` forgets the contexts whose tasks all ended, least recently changed first. |
| Tasks kept for one context | 100. Past that, `a2a-run` forgets the oldest tasks that ended. |
| Requests at once | 1024, including open streams |
| Egress proxy decision | The connection fails with 502 when the proxy doesn't connect and answer CONNECT within 10 seconds, or its answer head exceeds 16 KB |
| Egress proxy refusal | `a2a-run` reads 4 KB of the body and shows the agent the first 256 characters on one line |
| Egress proxy answer to an intercepted request | The head must arrive within 10 minutes and fit in 64 KB. The sandbox's own limits on intercepted requests also apply. |
| Idle connections to the egress proxy | 32, each closed after 90 seconds unused |
| Start of the session with the tools endpoint | 10 seconds |
| Answer to `hooks/beforeTool` | 30 seconds; a call without an answer in time is denied |
| `hooks/afterTool` notification | 10 seconds. Up to 64 wait to be sent; more are dropped. |
| Output of a tool call in `hooks/afterTool` | 64 KiB |
| An answer of the tools endpoint to `a2a-run` | 1 MiB |
| Requests of `a2a-run` to the tools endpoint at once | 16 |
| Check at start that the file server of a `--workspace` URL answers | 10 seconds |
| Workspace with `turn` sync | 1 GiB of files and 200000 entries, also for `--workspace memory` as a whole. A larger `dir:` workspace fails at start. A workspace that grows past them fails the turn until files are removed. |
| Files in the changes artifact | 1 MiB each and 4 MiB in total. Past that, the artifact lists the file without its content. |
| Paths that the changes artifact lists | 1000. Past that, it counts the rest. |
| Changes artifacts that wait for their task to end | 1024 or 64 MB. Past that, `a2a-run` drops the oldest. |
| Contacts | 100 |
| A response from a contact | 32 MiB, except event streams |
| Fetch of a contact's agent card | 10 seconds. `a2a-run` fetches the cards every hour, and a card that failed every minute. |
| Approvals of contacts that wait for the host | 1000. An approval is forgotten after 4 hours. |
| Wait for a contact's task after an answer to its approval | 1 hour |
| A `--contact-auth` value read from stdin with `-` | 64 KiB |

## Workspace protocol

These apply to the [workspace protocol](workspace-protocol.md) in `fsys.Handler` and `fsys.Client`.

| Limit | Value |
| --- | --- |
| A written file | 1 GiB |
| Entries of a listing, and changes in one answer | 200,000 |
| Wait of a request for changes | 1 minute |
| A path or a symlink target | 4096 bytes |
| JSON body of a request | 64 KiB |
| Requests a server answers at once | 256. Past that, a request fails with 503. |

## Live workspace

| Limit | Value |
| --- | --- |
| A file the agent has open for writing | 1 GiB. Past that, its writes fail with "file too large". |
| Files the agent has open for writing in one container, together | 2 GiB. Past that, their writes fail with "no space left on device". |
| Changes of the tree that the agent sees late | Up to 1 second, for an entry the agent looked at just before |
| Entries a removal of a directory checks for hidden ones | 200,000 |

## Logs

| Limit | Value |
| --- | --- |
| Server log | Rotated at 10 MB. The previous log is kept as `server.log.1`. |
