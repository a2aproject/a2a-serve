---
name: learn-environment
description: Learn the observability setup of this environment and propose the content of ENVIRONMENT.md. Use when asked to learn the environment, or when ENVIRONMENT.md is empty.
---

# Learn the environment

Collect, with cheap queries:

1. The datasources, by type and name.
2. The services: the values of the service label in metrics, logs and traces. Find which label names the service, environment, cluster and version.
3. The dashboards, by folder, and the ones that look like service overviews.
4. The alert rules and their labels, and the ones that fire often.
5. The clusters and namespaces, when the `kubernetes` tool is on.
6. Where deploys show up: annotations, Kubernetes rollouts, or GitHub workflow runs.

Then propose the full content of `ENVIRONMENT.md`, following its template, in your answer. Mark what you guessed. Don't edit the file; the owner does.
