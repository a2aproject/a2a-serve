# Inspector

`a2a-serve inspect` opens a web page with the agent's conversations, tasks, pending approvals and status. You can answer approvals there.

The login link works once, for 5 minutes, and only someone with access to the agent directory can get one. The page is on the local URL. `inspect --url private` opens it on the private URL of a tailnet, which uses HTTPS. Relays and the public URL don't serve it, because its login link and session would cross the network unencrypted.

Not in this version: files, live logs, backups, and changing settings from the page.
