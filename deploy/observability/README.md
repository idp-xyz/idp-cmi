# idp-cmi observability

`alerts.yaml` holds Prometheus rules (plain rule-file format); `alerts_test.yaml` is their unit test:

    docker run --rm -v "$PWD:/r:ro" --entrypoint sh prom/prometheus:v3.14.0 -c 'cd /r && promtool check rules alerts.yaml && promtool test rules alerts_test.yaml'

`go test ./tests/deploy` also checks that every `idp_cmi_` metric the rules read is exported by the code.

## Service level objectives (30 days)

| SLI | Objective | Where |
|---|---|---|
| Requests that do not fail with 5xx / all requests | 99.5 % | `idp_cmi:availability:ratio_rate1h`, burn alerts |
| p95 latency of `POST /v1/context/resolve` | under 1 s | `idp_cmi:resolve_latency_p95:5m` |
| Age of the oldest unprocessed embedding projection event | under 10 min | `idp_cmi_projection_oldest_age_seconds` |

The objectives are starting values chosen without production traffic; revisit them after a month of real data.

## Metrics added for operations

| Metric | Meaning |
|---|---|
| `idp_cmi_memory_writes_total{outcome}` | `stored`, `flagged` (stored but quarantined by the injection scan), `rejected` (invalid, unregistered source, ceiling), `quota`, `rate_limited` |
| `idp_cmi_projection_pending`, `idp_cmi_projection_oldest_age_seconds` | embedding outbox backlog across tenants |
| `idp_cmi_db_up` | 1 when the replica can reach PostgreSQL |

## Not covered here

The database backup is a Kubernetes CronJob, not part of this service; alert on it where the CronJob is deployed
(kube-state-metrics: `time() - kube_cronjob_status_last_successful_time{cronjob="pg-backup"} > 7200`).
