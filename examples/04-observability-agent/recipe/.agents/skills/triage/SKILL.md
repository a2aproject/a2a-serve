---
name: triage
description: Investigate an alert, or a report that a service is slow, failing or down. Use for incidents and "what is wrong with X" questions.
---

# Triage

Work through these steps, and stop as soon as the evidence is clear. Report what you checked, also when it found nothing.

1. **Scope and onset.** Which service, which environment, since when. Find the onset in the service's request rate, error rate and latency (RED) over the last few hours.
2. **Alerts.** List the firing alert rules for the service and its dependencies.
3. **Dependencies.** Check the RED metrics of the services it calls, from `ENVIRONMENT.md`. A dependency that degraded first is the likelier cause.
4. **What changed at onset.** Deploys and rollouts (Kubernetes events, GitHub workflow runs and merged pull requests, Grafana annotations), config changes, and traffic shifts.
5. **Logs at onset.** Loki patterns for the service around the onset; then error lines, filtered by labels. Note trace IDs.
6. **Traces.** Slow or failing traces of the service around the onset, and the span where time or the error comes from.
7. **Saturation.** CPU, memory, restarts and OOM kills, connection pools, queue depth.
8. **Conclusion.** The likely cause with its evidence, your confidence, what would confirm it, and a suggested mitigation for people to apply.
