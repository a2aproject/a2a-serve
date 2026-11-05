---
name: logql
description: Write LogQL for log questions against Loki datasources.
---

# LogQL

- Build the stream selector from label discovery, such as `{service="checkout", env="prod"}`. Never query without one.
- Filter lines before parsing: `|= "error"`, `!= "healthcheck"`, `|~ "timeout|deadline"`.
- Parse only when you need fields: `| json` or `| logfmt`, then filter on a field: `| status >= 500`.
- Use patterns to see the shapes of the log lines before reading raw lines.
- Trends are metric queries: `sum by (level) (count_over_time({service="checkout"} |= "error" [5m]))`.
- Ask for few lines, such as 50, and narrow the time range before asking for more.
- Note trace IDs in the lines, to look the traces up.
