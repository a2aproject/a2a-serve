# Email Sign-In

People on an email allowlist can call the agent with no pairing code. On the public URL they sign in with Google. The allowlist is `--allow-email`:

```sh
a2a-serve config set --allow-email mom@example.com,'*@example.com'
a2a-serve config set --google-client-id env:GOOGLE_CLIENT_ID \
  --google-client-secret env:GOOGLE_CLIENT_SECRET
a2a send -a https://coder.example.com "hi"      # signs in once
```

Add `<agent-url>/oauth/callback` to the authorized redirect URIs of your Google app. Use the URL people call, usually the public one.

`*@example.com` admits everyone of the example.com Google Workspace, and anyone who can send verified mail for that domain counts as any of its addresses. Tailnet and relay logins match only full addresses.

People who sign in are [members](identities.md#roles). Removing an email from the allowlist cuts them off. A sign-in lasts as long as the person keeps using it; see [limits](../reference/limits.md#sign-in). To revoke one person's sign-ins, run `a2a-serve pair rm --email anna@example.com`. They can sign in again while the allowlist matches their email.

Only the public URL (Funnel or a relay) serves sign-in. On the local URL and the private network, people use pairings or their network identity.

Not in this version: providers other than Google, and sign-in without your own Google app.
