# HTTP routes

An a2a-serve agent serves these routes on every listener.

| Route | |
| --- | --- |
| `GET /`, `GET /.well-known/agent-card.json` | the agent card, which names the `pairing` bearer scheme where callers need a pairing: on the local URL, on the private URL with `--private-access paired`, and on the public URL unless `--public-access anyone` |
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

## Browser requests

State-changing requests (any method except `GET`, `HEAD` and `OPTIONS`) to the A2A routes, `/pair`, `/n/…` and `/mailbox/api/…` need `Content-Type: application/json` or `application/a2a+json`, or the `A2A-Version` header. Others get 415. A browser sends such a request to another site only after a CORS preflight, and the server allows CORS only for the agent card.

On every route, the server refuses state-changing requests that a browser marks as cross-site, with `Sec-Fetch-Site` or an `Origin` that doesn't match the host. They get 403. The forms of `/hitl/<token>` and the inspector rely on this check, and the inspector's forms also carry a CSRF token.

The LAN listener accepts only requests for its own address in the `Host` header, and answers others with 421. This keeps a web page that rebinds its DNS name to the LAN address from passing the checks above.

## Signatures and headers

The server wakes a webhook node with a POST signed in `X-A2A-Serve-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. Calls between agents carry `X-A2A-Serve-Hops`, the number of agent servers a call passed through. A call with a count of 1 or more gets at most the authenticated role.
