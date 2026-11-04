# Email Sign-In

People on an email allowlist can call the agent with no pairing code. On the public URL they sign in with Google. The allowlist is `--allow-email`, and the emails of `--owner` and `--allow`:

```sh
a2a-serve config set --allow-email mom@example.com,'*@example.com'
a2a-serve config set --google-client-id env:GOOGLE_CLIENT_ID \
  --google-client-secret env:GOOGLE_CLIENT_SECRET
a2a send -a https://coder.example.com "hi"      # signs in once
```

- Add `<agent-url>/oauth/callback` to the authorized redirect URIs of your Google app. Use the URL people call, usually the public one.
- `*@example.com` matches accounts of the example.com Google Workspace, and mail from example.com that passes DMARC. Anyone can create a personal Google account with an @example.com address, so such accounts don't match. Tailnet and relay logins match only full addresses.
- People who sign in get the role their email has: authenticated, or owner with `--owner email:<address>`. Their tokens carry the email, so removing it from the allowlist cuts them off at their next request.
- Only the public URL serves sign-in: Funnel or a relay. On the local URL and the private network, people use pairings or their network identity.
- The agent card on the public URL names the sign-in scheme, which tells clients how to sign in. See [Sign-in schemes](../reference/http-routes.md#sign-in-schemes).
- `--google-client-id` needs an email on the allowlist.

Not in this version: providers other than Google, and sign-in without your own Google app.
