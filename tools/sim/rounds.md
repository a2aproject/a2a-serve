# Rounds

One line per simulation round, newest last. See `SKILL.md`.

| Date | Commit | Lanes | Novelty | Findings | Issues |
| --- | --- | --- | --- | --- | --- |
| 2026-11-06 | 3604240, rechecked on 1994e97 | acp, approvals, multiuser, cli, resilience, concurrency | first round; opencode with a Gemini model, Docker sandbox | 70 raw, about 60 distinct; 3 fixed on 1994e97 | a2a-serve #161-#180, a2a-go #549-#551, a2a-cli #136-#137 |
| 2026-11-06 | 44ab8d1 | multiuser, peers-mesh, peers-calls | agentmesh build with loopback relays; model google/gemini-3-flash-preview; personas; chaos off | not recorded | a2a-serve #234-#244, a2a-go #561, a2a-cli #139-#140 |
| 2026-11-07 | 9341d2f | acp, approvals, multiuser, cli, http (first run), wildcard (recipes, MCP, card skills, schedules); resilience and concurrency in a second run directory | chaos on the second directory only (seed 54326); personas; models gemini-3-flash-preview and gemini-flash(-lite)-latest | 69 raw, about 55 distinct; 15 nitpicks or intended behaviors added to review/baseline.md; 2 kept for a private report | a2a-serve #285-#295, a2a-go #564, a2a-cli #144; recheck comments on #167, a2a-go#561, a2a-cli#136, #137, #139, #140 |
| 2026-11-09 | 52d8d60 | acp, approvals, multiuser, cli, http, wildcard (sandbox egress); resilience and concurrency (multi-agent) in a second run directory; multiuser-2 and sandbox-2 reruns; examples-a (01, 02 partial), examples-b (03, 04, 05 partial) | chaos on the second directory only (seed 50582); personas; first round with egress through the server (CGO_ENABLED=0 build); /tmp full during wave 1 | 63 raw, about 45 distinct, 1 high (security) | a2a-serve #389-#391 |
