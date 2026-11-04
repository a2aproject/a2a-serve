# HTTP routes

Served on every listener:

| Route | |
| --- | --- |
| `GET /`, `GET /.well-known/agent-card.json` | the agent card |
| `POST /` | A2A JSON-RPC |
| other paths | A2A HTTP+JSON (REST) |
| `POST /pair` | exchange a pairing code for a caller or node token |
| `/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`, `/oauth/…` | Google sign-in, when `--allow-email` is set |
| `/hitl/<token>` | answer a question in a browser |
| `/inspect/…` | the owner's web inspector (`a2a-serve inspect`); local and private listeners only |
| `/n/<node>/…` | webhook nodes |
| `/mailbox/api/…` | the node API used by `a2a-serve mailbox` |
| `GET /skill.md`, `GET /mailbox/skill.md` | skills for agents calling this one (the `a2a skill` of the server's `a2a`), and for nodes |
| `GET /download/a2a`, `GET /download/a2a-serve` | the server's `a2a` and `a2a-serve` binaries, for nodes |

The server wakes a webhook node with a POST signed in `X-A2A-Serve-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`. Calls between your own agents carry `X-A2A-Serve-On-Behalf-Of`, naming the user a call is made for, and `X-A2A-Serve-Hops`.
