---
name: promql
description: Write PromQL for metric questions against Prometheus datasources.
---

# PromQL

- Discover first: list metric names with a filter, then the label names and values of the metric you picked.
- Counters (`_total`, `_count`, `_sum`) need `rate()` or `increase()`, over a range of at least four scrape intervals, such as `[5m]`.
- Aggregate with `sum by (label)`, keeping only the labels you need.
- Error ratio: `sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))`.
- Latency from histograms: `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`. Keep `le` in the `by` clause.
- Compare with a baseline: the same expression with `offset 1d` or `offset 1w`.
- Use range queries with a step that gives about 100 to 300 points, and instant queries for current values.
