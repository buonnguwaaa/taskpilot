---
name: prod-logs
description: How to read production logs in Splunk for the BOM and payment projects (PLM has no production logs) - namespaces, service/container names, log fields, request_id rules, known gaps and investigation recipes. Use before calling splunk_search for a production bug or incident.
---

# Production logs

Production logs live in Splunk (`siem.hasaki.work`) and are searched with the
`splunk_search` tool (read-only, scoped to the task's project). QC is different:
use `qc_apps` (deployment state from Argo CD) and `qc_logs` (pod logs from Argo
CD) for all three projects, including PLM. Read the file
for the task's project before searching:

- payment → `payment.md` (in this skill's directory)
- bom → `bom.md`
- plm → no production logs. Do not search Splunk for PLM tasks.

General rules:

- Start narrow: one container, `level=error`, `-1h`. Widen the time range or
  drop filters only when the narrow search finds nothing.
- Prefer exact identifiers (`request_id`, transaction/refund/order code)
  over free text.
- No log line does not prove nothing happened: some services lose logs, and
  Kafka/worker flows carry no `request_id` (see the project file).
- Logs contain personal and payment data. Quote only what the investigation
  needs (IDs, error messages, timestamps) in plans and PRs; never copy payloads
  into code, commits or tickets.
