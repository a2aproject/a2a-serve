---
name: traceql
description: Write TraceQL for latency and error questions against Tempo datasources.
---

# TraceQL

- Discover attribute names and values first.
- Failing requests: `{ resource.service.name = "checkout" && status = error }`.
- Slow requests: `{ resource.service.name = "checkout" && duration > 2s }`.
- Narrow to an endpoint: `{ span.http.route = "/api/pay" }`, combined with the above.
- Metrics from spans: `{ resource.service.name = "checkout" } | quantile_over_time(duration, .99)` or `| rate()`.
- Fetch one or two traces by ID and find the span that holds most of the time or the error. Compare a slow trace with a fast one of the same endpoint.
