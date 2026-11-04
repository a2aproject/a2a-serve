# HTTP Routes

An a2a-serve agent serves these routes on every listener.

| Route | |
| --- | --- |
| `GET /`, `GET /.well-known/agent-card.json` | the agent card, which names the `pairing` bearer scheme where callers need a pairing: on the local URL, on the private URL with `--private-access paired`, and on the public URL unless `--public-access anyone`. On the public URL it also names the [sign-in scheme](#sign-in-schemes) |
| `POST /` | A2A JSON-RPC |
| other paths | A2A HTTP+JSON (REST) |
| `POST /pair` | exchange a pairing code for a caller or node token |
| `/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`, `/oauth/…` | Google sign-in on the public URL, when `--allow-email`, `--owner` or `--allow` names an email |
| `/hitl/<token>` | answer a question in a browser |
| `/inspect/…` | the owner's web inspector (`a2a-serve inspect`); on the local listener, and on the private listener of a tailnet |
| `/n/<node>/…` | webhook nodes |
| `/mailbox/api/…` | the node API used by `a2a-serve mailbox` |
| `GET /skill.md`, `GET /mailbox/skill.md` | skills for agents calling this one (the `a2a skill` of the server's `a2a`), and for nodes |
| `GET /download/a2a`, `GET /download/a2a-serve` | the server's `a2a` and `a2a-serve` binaries, for nodes |

## Sign-in schemes

On the public URL, the agent card names a `google` scheme when sign-in is on. It is an alternative to pairing, so the card's security requirements are `[{pairing}, {google}]`.

| Settings | `google` scheme | Client flow |
| --- | --- | --- |
| `--allow-email` | `openIdConnect` with `https://accounts.google.com/.well-known/openid-configuration` | Google sign-in with the shared app, then the token exchange at `/oauth/token` |
| `--allow-email`, `--google-client-id`, `--google-client-secret` | `oauth2` authorization code flow with PKCE: `authorizationUrl` `<url>/oauth/authorize`, `tokenUrl` `<url>/oauth/token`, `oauth2MetadataUrl` `<url>/.well-known/oauth-authorization-server` | a loopback redirect against the agent |
| neither | none | `a2a-serve pair join` |

With the shared app, the client puts `nonce = base64url(SHA-256(<agent URL>))` in its Google sign-in request, with the agent URL's trailing slash removed. It then sends the ID token to `/oauth/token` with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token=<ID token>` and `subject_token_type=urn:ietf:params:oauth:token-type:id_token`. The agent checks the nonce and returns its own access token.

Every agent accepts ID tokens of the shared app. The nonce binds an ID token to one agent, so that agent can't replay it at another. Google drops the nonce from refreshed ID tokens, so the client exchanges the ID token once and uses the agent's token from then on. With the owner's own app, the Google ID token stays in the agent, which needs no binding. The agent keeps accepting the token exchange then.

The shared app is planned: until it is registered, the card names no scheme without `--google-client-id`.

## Browser requests

State-changing requests (any method except `GET`, `HEAD` and `OPTIONS`) to the A2A routes, `/pair`, `/n/…` and `/mailbox/api/…` need `Content-Type: application/json` or `application/a2a+json`, or the `A2A-Version` header. Others get 415. A browser sends such a request to another site only after a CORS preflight, and the server allows CORS only for the agent card.

On every route, the server refuses state-changing requests that a browser marks as cross-site, with `Sec-Fetch-Site` or an `Origin` that doesn't match the host. They get 403. The forms of `/hitl/<token>` and the inspector rely on this check, and the inspector's forms also carry a CSRF token.

## Signatures and headers

The server wakes a webhook node with a POST signed in `X-A2A-Serve-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. Calls between agents carry `X-A2A-Serve-Hops`, the number of agent servers a call passed through. A call with a count of 1 or more gets at most the authenticated role.
