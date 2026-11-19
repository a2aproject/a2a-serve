# Workspace Protocol

This page specifies the HTTP protocol through which a program serves the agent's workspace as a file tree, so that the agent works on the files that the program holds.

The protocol maps one to one onto the Go interface `fsys.FS` in `core/fsys`. In Go, `fsys.Handler` serves a tree with it and `fsys.Client` reads one. A server in any language implements the requests below. [`a2a-run --workspace <url>`](a2a-run.md#workspace) takes the URL of such a server.

## Transport

The server listens on a Unix socket, `unix:///path/to/socket`, or on HTTP, `http://host:port` or `https://host:port` with an optional path prefix. Requests are plain HTTP/1.1. The protocol has no authentication, so serve it only on a Unix socket or a loopback address.

## Paths

A path is relative to the root of the tree, slash-separated and cleaned, such as `src/main.go`. `.` is the root. The server refuses a path that is empty, absolute, contains NUL, or isn't clean, such as `a/../b`, `./a`, `a/` or `..`, with `400` and the code `invalid`. The client refuses such paths before it sends them, and refuses entries outside the directory it listed.

No request follows a symlink. A path through one doesn't exist, and a request that creates an entry under one gets `422` with the code `not_dir`.

## Entries

`stat`, `list`, writes and changes describe an entry as a JSON object:

```json
{"path":"src/main.go","type":"file","mode":420,"size":12,"modTime":"2026-11-19T16:02:00.123456789Z","version":"9f1c.4"}
```

| Field | Value |
| --- | --- |
| `path` | The path of the entry |
| `type` | `file`, `dir` or `symlink` |
| `mode` | The permission bits as a number, such as 420 for `0644` and 493 for `0755` |
| `size` | The size of a file, in bytes |
| `modTime` | The time of the last change, in RFC 3339 |
| `version` | A string that changes on every write of the entry. The server picks its format, without `"`. |
| `target` | The target of a symlink |
| `removed` | `true` for an entry that `changes` reports as removed, which has only `path` besides |

Fields with a zero value may be left out.

## Requests

| Request | Body | Answer |
| --- | --- | --- |
| `GET /stat?path=P` | | `200` with the entry at `P`. A symlink isn't followed. |
| `GET /list?path=P` | | `200` with `{"entries":[...]}`, the entries of the directory `P` sorted by name |
| `GET /file?path=P` | | `200` with the bytes of the file `P`, and its version in quotes in `ETag` |
| `PUT /file?path=P&mode=0644` | The new contents | `200` with the entry written. It replaces the file or creates it, and creates the missing parent directories. `mode` is octal. |
| `DELETE /file?path=P` | | `204`. It removes the entry, a directory with its contents. |
| `POST /mkdir` | `{"path":"P","mode":493}` | `204`. It creates the directory and its missing parents. A directory already there isn't an error. |
| `POST /symlink` | `{"target":"T","path":"P"}` | `204`. It creates a symlink and the missing parent directories. |
| `POST /rename` | `{"from":"A","to":"B"}` | `204`. It moves an entry with its contents. A file replaces a file at `B`; other entries there fail the request. |
| `GET /changes?cursor=C&wait=MS` | | `200` with `{"changes":[...],"cursor":"N"}` |

### Ranges

`GET /file` takes one range, `Range: bytes=FIRST-` or `Range: bytes=FIRST-LAST`, and answers `206` with `Content-Range`. A range that starts past the end of the file gets `416`.

### Conditional writes

`PUT /file` with `If-Match: "VERSION"` writes only over the entry with that version, and `If-None-Match: *` only creates the file. A write that doesn't match gets `412` with the code `conflict`, and changes nothing. Without either header it writes unconditionally.

### Changes

`GET /changes` returns the entries that changed since `cursor`, the latest state of each, and the cursor to pass next. The cursor is opaque. An empty cursor returns no changes and the current cursor.

When nothing changed, the server waits for a change up to `wait` milliseconds, then answers with no changes and the same cursor. When the server no longer has the changes since `cursor`, or doesn't know the cursor, it answers `410` with the code `resync` and the current cursor in `cursor`. The client then reads the tree again and continues from that cursor.

## Errors

A failed request has a JSON body with a message and a code:

```json
{"error":"stat src/missing.go: file does not exist","code":"not_exist"}
```

| Status | Code | Meaning |
| --- | --- | --- |
| 400 | `invalid` | An invalid path, mode, range or body |
| 403 | `permission` | The tree refuses the change, such as of a read-only workspace |
| 404 | `not_exist` | No entry at the path |
| 409 | `exist` | An entry is already at the path |
| 410 | `resync` | The cursor is too old; `cursor` has the current one |
| 412 | `conflict` | The entry changed since the version of the write |
| 413 | `too_large` | The body or the answer is over a [limit](limits.md#workspace-protocol) |
| 422 | `is_dir`, `not_dir`, `not_empty`, `cross_device` | The entry is a directory, isn't one, isn't empty, or can't move there |
| 503 | `busy` | Too many requests at once |

Other failures get `500`.

## Conformance

The conformance suite of `a2a-run` checks a server from a checkout of this repository:

```sh
go test ./internal/a2arun/conformance -workspace unix:///path/to/socket
```

It works in a directory of its own in the tree, `a2a-run-conformance-` and a random suffix, and removes it at the end. The reference host serves a tree in memory on the same socket as its egress proxy and tools endpoint:

```sh
go run ./internal/a2arun/refhost/cmd/refhost --listen unix:///tmp/host.sock --workspace
```

Pass `unix:///tmp/host.sock` to `--workspace`, `--tools` and `--egress-proxy` alike. The reference host logs each change of its tree on stderr.
