# Runbook — slow requests

## Detection

```promql
histogram_quantile(0.95,
  sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
```

Or client reports of timeouts, or a rise in `503 request_timeout`.

## Likely causes

1. Connection pool saturation — usually the answer
2. A slow query, often a missing index after a data change
3. The database is under-resourced
4. Genuine traffic growth
5. A slow OTLP collector (should be impossible — the exporter is asynchronous)

## Diagnosis

### 1. Which route?

```promql
histogram_quantile(0.95,
  sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
```

- **All routes slow** → pool or database.
- **One route slow** → that query.
- **Only `POST /appointments`** → write contention. Check the
  [conflicts runbook](booking-conflicts.md).

### 2. Is the pool the bottleneck?

This is the first thing to check, because it is the most common cause and the
easiest to confirm:

```promql
db_pool_acquired_connections / db_pool_max_connections
rate(db_pool_acquire_duration_seconds_total[5m])
rate(db_pool_empty_acquire_total[5m])
```

`rate(db_pool_acquire_duration_seconds_total[5m])` is seconds spent **waiting**
per second of wall time. If it is meaningfully above zero, requests are queueing
for a connection and the database itself may be perfectly healthy. Go to
[database-unavailable.md](database-unavailable.md#pool-exhausted).

### 3. Is a query slow?

```sql
SELECT pid, now() - query_start AS duration, state, left(query, 120)
FROM pg_stat_activity
WHERE datname = current_database() AND state <> 'idle'
ORDER BY duration DESC LIMIT 10;
```

If `pg_stat_statements` is available:

```sql
SELECT mean_exec_time, calls, left(query, 120)
FROM pg_stat_statements ORDER BY mean_exec_time DESC LIMIT 10;
```

Check the expected indexes are being used:

```sql
EXPLAIN ANALYZE
SELECT starts_at FROM appointments
WHERE doctor_id = '…' AND status = 'booked'
  AND starts_at >= '…' AND starts_at < '…';
-- should use appointments_doctor_starts_at_idx
```

A sequential scan on `appointments` means an index is missing or was not used —
check that the partial index predicate still matches the query.

### 4. Use a trace

If tracing is enabled, one slow request's trace shows exactly where the time went
— HTTP handler versus SQL. That is faster than any of the above.

```bash
railway logs --environment production \
  | grep 'event=http.request' | grep -v 'duration_ms=[0-9]\{1,2\}"' | tail
```

Take a `trace_id` and open it in Grafana/Tempo.

## Mitigation

| Cause | Immediate | Structural |
|---|---|---|
| Pool saturated | Raise `DB_MAX_CONNS` **if the server limit allows** | Add replicas, or PgBouncer |
| Slow query | Kill it: `pg_cancel_backend(pid)` | Add an index; consider partitioning |
| Under-resourced database | — | Upgrade the plan |
| Traffic growth | Scale replicas | Capacity planning |

Before raising `DB_MAX_CONNS`, check the budget: `DB_MAX_CONNS × replicas` must
fit inside `max_connections`. Raising it past that makes things **worse**.

## Escalation

Escalate if p95 stays high with the pool healthy and no slow queries — that
points at the platform.

## Verify recovery

```promql
histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))
rate(db_pool_acquire_duration_seconds_total[5m])
```

Both back to baseline, and `503 request_timeout` responses gone.
