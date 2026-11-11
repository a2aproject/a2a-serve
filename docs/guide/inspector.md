# Inspector

`a2a-serve inspect` opens a web page with the agent's conversations, tasks, pending approvals and status. You can answer approvals there.

The login link works once and only for a few minutes. The session ends after a while or when the server restarts; run `a2a-serve inspect` again for a new link.

The page is on the local URL. `a2a-serve inspect --url private` opens it on the private URL of a tailnet. With Headscale that URL is plain HTTP, so open it only on a device that uses MagicDNS (see [Networks](networks.md#tailscale)). Relays and the public URL don't serve the inspector.

Not in this version: files, live logs, backups, and changing settings from the page.
