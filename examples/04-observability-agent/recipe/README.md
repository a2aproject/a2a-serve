# Observability agent recipe

An agent that answers questions about production and investigates incidents with your metrics, logs, traces and profiles through Grafana, and optionally your Kubernetes cluster and GitHub repositories.

Create it with `a2a-serve recipe`. The [walkthrough](../README.md) covers the whole setup.

## Access it needs

Anyone who can call the agent can make it read what its credentials can read. Give it read-only identities with the access you would give its callers.

| Server | Variables | Identity |
| --- | --- | --- |
| grafana, required | `GRAFANA_URL`, `GRAFANA_SERVICE_ACCOUNT_TOKEN` | A Grafana service account with the Viewer role. The server runs with `--disable-write`. |
| github, optional | `GITHUB_TOKEN` | A fine-grained token with read access to contents, pull requests and actions of your service repositories. The server is GitHub's read-only endpoint. |
| kubernetes, optional | none | A kubeconfig in `~/.kube/a2a-o11y/config` for a ServiceAccount bound to the built-in `view` ClusterRole, and `~/.kube/a2a-o11y/config.toml` with `read_only = true`. |

The agent also needs a model API key, which `a2a-serve recipe` asks for.

## What it can do

It reads. It can't edit its workspace, run commands or fetch URLs: the recipe denies those tool kinds. It asks before each MCP call; see the walkthrough for how to answer fewer questions.
