# HTTP Routes

An a2a-serve agent serves these routes on every listener, and a relay serves the [relay routes](#relay-routes).

| Route | |
| --- | --- |
| `GET /`, `GET /.well-known/agent-card.json` | the agent card, which names the `pairing` bearer scheme where callers need a pairing: on the local URL, on the private URL with `--private-access paired`, and on the public URL unless `--public-access anyone`. On the public URL it also names the [sign-in scheme](#sign-in-schemes) |
| `POST /` | A2A JSON-RPC |
| other paths | A2A HTTP+JSON (REST) |
| `POST /pair` | exchange a pairing code for a caller or node token |
| `/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`, `/oauth/…` | Google sign-in on the public URL, when `--allow-email` is set |
| `/hitl/<token>` | answer a question in a browser |
| `/inspect/…` | the owner's web inspector (`a2a-serve inspect`); on the local listener, and on the private listener of a tailnet |
| `/n/<node>/…` | webhook nodes. The card of a node is public only if the node is [advertised](../guide/webhook-nodes.md#advertised-pairings); other cards go to callers who may call the node |
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

The agent card and the cards of nodes allow pages of any origin to read them. Their responses carry `Access-Control-Allow-Origin: *` and allow no credentials. The server answers a CORS preflight (`OPTIONS`) for them with `204`, so a page can send headers such as `A2A-Version` with the request.

On every route, the server refuses state-changing requests that a browser marks as cross-site, with `Sec-Fetch-Site` or an `Origin` that doesn't match the host. They get 403. The forms of `/hitl/<token>` and the inspector rely on this check, and the inspector's forms also carry a CSRF token.

## Request limits

Each listener handles at most 1024 requests at once, including open streams. It answers requests over this limit with 503 and `Retry-After`.

`/pair`, `/oauth/…` and `/hitl/<token>` serve callers before they authenticate. They share 256 of those slots, and one client may hold 32 of them. The server tells clients apart as it does for failed pairing attempts, so anonymous callers through the relay share one client. A request over the client's limit gets 429, and a request over the shared limit gets 503. Both carry `Retry-After`.

| Route | Largest body | Time to send the body |
| --- | --- | --- |
| `POST /pair`, `/hitl/<token>` | 4 KiB | 10 seconds in total |
| `POST /oauth/token` | 64 KiB | 10 seconds in total |
| A2A routes, `/n/…` | 32 MiB | 30 seconds without progress |

A body over the limit gets 413. When the time runs out, the request fails and the server closes the connection. The timeouts end once the body is read, so a response stream that follows can stay open.

## Signatures and headers

The server wakes a webhook node with a POST signed in `X-A2A-Serve-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. The key is the hex HMAC-SHA256 of `a2a-serve wake signing key` under the node token, so a node checks wakes with its token alone, using `a2a-serve mailbox verify`. Retries of a wake keep its `X-A2A-Serve-Wake` ID. Calls between agents carry `X-A2A-Serve-Hops`, the number of servers a call passed through. A call with a count of 1 or more gets at most the authenticated role.

## Relay routes

A relay that `a2a-serve relay run` runs serves these routes on its port.

| Route | |
| --- | --- |
| `GET /.well-known/a2a-serve-relay` | the relay's versions, and whether people may sign in: `{"a2a_serve": "<version>", "agentmesh": "<version>", "signin": true}` |
| `/a2a-serve/control/…` | the relay's control API: `GET status`, `POST invites`, `GET members`, `POST members/remove`. It needs the admin token as a bearer token. After a wrong token, the next wrong tokens get 429 with `Retry-After` for 1 to 10 seconds |
| `/a2a-serve/signin/…` | the relay's identity provider, an OpenID Connect issuer at `<relay-url>/a2a-serve/signin` with the client ID `a2a-serve`: `GET .well-known/openid-configuration`, `GET jwks`, the device flow (RFC 8628) at `POST device/authorize` and `POST token`, which also takes the `refresh_token` grant, and the pages people sign in on: `GET device`, `GET callback` and `POST approve`. ID tokens are signed with EdDSA and last an hour. 404 when sign-in is off |
| `/admin/…`, `POST` and `PUT /policies`, `/console/…`, `/metrics` | 404 |
| other paths | Agent Mesh's control plane, routers and ingress, such as `/info`, `/register`, `/refresh`, `/readyz` and `/ingress/…`, with the path cleaned of dot segments. They answer 503 while the relay starts. `CONNECT`, `TRACE` and other methods that Agent Mesh doesn't serve get 405 |
