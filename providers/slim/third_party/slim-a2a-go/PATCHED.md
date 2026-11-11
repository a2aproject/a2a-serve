# Patched slim-a2a-go v0.5.2

This is a copy of the `a2apb/v1`, `a2aslimrpc/v1` and `internal/caller`
packages of [slim-a2a-go](https://github.com/agntcy/slim-a2a-go) v0.5.2
(Apache-2.0, see LICENSE). The v0 packages and the tests are left out.

slim-a2a-go v0.5.2 is built against a2a-go v2.1.0 and doesn't compile against
a2a-go v2.7, which a2a-serve uses: v2.7 replaced `CreateTaskPushConfigRequest`
with `PushConfig`, and `RequestHandler.ListTaskPushConfigs` now returns a
`*ListTaskPushConfigResponse`. The patch adapts `converter.go`, `client.go` and
`handler.go` to that. The wire format is unchanged.

Remove this directory and the `replace` in `../../go.mod` once an upstream
release supports a2a-go v2.7.

The generated client stubs in `a2apb/v1` passed a negative timeout to the
bindings when the context's deadline had already passed, which panics. They
return `context.DeadlineExceeded` instead.
