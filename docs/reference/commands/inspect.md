# a2a-serve inspect

<!-- Generated from the command definitions; do not edit. -->

Open a page where the owner sees what the agent is doing: its conversations
and tasks, pending approvals with Approve and Deny, and the state of its
messengers, pairings, webhook nodes and contacts.

The link logs in once and works for 5 minutes; the session lasts 12 hours.
The page is served on the local URL, and with --private on the private URL
of a tailnet, which is HTTPS. It is never served on the LAN, a relay or the
public URL.

```
a2a-serve inspect [flags]
```

**Flags**

| Flag | Default | Description |
|---|---|---|
| `--dir string` |  | Agent directory (default: the nearest directory with .a2a, walking up from the current one) |
| `--no-browser` |  | Print the link without opening a browser |
| `--private` |  | Use the agent's URL on its tailnet |
| `--timeout duration` | `1m0s` | Timeout of each call to the running agent, a duration such as 30s or 2m |
