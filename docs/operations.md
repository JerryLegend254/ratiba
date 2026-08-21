# Operations

Running, observing and debugging Ratiba. Incident procedures are in
[runbooks/](runbooks/).

---

## Configuration reference

Configuration is read **once at startup** into an immutable struct. Nothing else
in the codebase calls `os.Getenv`. A bad value stops the process immediately with
every problem listed at once, rather than failing hours later on a rarely-taken
path.

Legend — **Req**: required · **Sec**: secret, never log or commit.

### Core

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `APP_ENV` | Deployment tier | | | `development` | `development` \| `staging` \| `production` \| `test`. Production enables extra safety checks |
| `SERVICE_NAME` | Identity in logs, metrics, traces | | | `ratiba-api` | |
| `PORT` | Listening port | | | `8080` | Railway injects this — do not set it there |

### Database

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `DATABASE_URL` | Connection string | **yes** | **yes** | — | On Railway use `${{Postgres.DATABASE_URL}}`, not a literal, so a rotated password propagates without a redeploy |
| `DB_MAX_CONNS` | Pool ceiling | | | `10` | See [connection budget](#connection-budget) |
| `DB_MIN_CONNS` | Warm connections | | | `2` | Must be ≤ max |
| `DB_MAX_CONN_LIFETIME` | Recycle age | | | `30m` | Jittered by 10% so replicas do not recycle in lockstep |
| `DB_MAX_CONN_IDLE_TIME` | Idle reap | | | `5m` | |
| `DB_CONNECT_TIMEOUT` | Dial timeout | | | `5s` | |
| `DB_STATEMENT_TIMEOUT` | Server-side cap | | | `8s` | PostgreSQL cancels the query itself — the backstop for a leaked context |

### HTTP

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `HTTP_READ_HEADER_TIMEOUT` | Header deadline | | | `5s` | The specific defence against a Slowloris-style hold |
| `HTTP_READ_TIMEOUT` | Whole-request read | | | `15s` | |
| `HTTP_WRITE_TIMEOUT` | Response write | | | `20s` | |
| `HTTP_IDLE_TIMEOUT` | Keep-alive idle | | | `60s` | |
| `HTTP_HANDLER_TIMEOUT` | Handler budget | | | `10s` | **Must be < `HTTP_WRITE_TIMEOUT`**, or the timeout response cannot be written. Validated at startup |
| `HTTP_SHUTDOWN_TIMEOUT` | Drain budget | | | `20s` | Should be ≤ the platform's grace period |
| `HTTP_MAX_BODY_BYTES` | Body limit | | | `65536` | The largest legitimate body is a few hundred bytes |

### Logging

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `LOG_LEVEL` | Verbosity | | | `info` | Production refuses `debug` |
| `LOG_FORMAT` | `json` or `text` | | | `text` in dev, `json` elsewhere | Production refuses `text` |

### Observability

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `METRICS_ENABLED` | Serve `/metrics` | | | `true` | |
| `METRICS_AUTH_TOKEN` | Bearer token for `/metrics` | prod | **yes** | *(empty)* | **Mandatory in production** — Railway routes every path publicly. `openssl rand -hex 32` |
| `OTEL_TRACES_ENABLED` | Enable tracing | | | `false` | |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector URL | | | *(empty)* | Empty ⇒ tracing is a no-op rather than a failure |
| `OTEL_EXPORTER_OTLP_INSECURE` | Plain HTTP | | | `true` | Fine for a collector on a private network |
| `OTEL_TRACES_SAMPLER_ARG` | Head sampling ratio | | | `1.0` | Lower under real traffic |
| `PPROF_ENABLED` | Runtime profiles | | | `false` | **Refused in production.** Binds its own listener, never the API router |
| `PPROF_ADDR` | pprof listener | | | `127.0.0.1:6060` | Loopback by default |

### Booking

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `BOOKING_MIN_LEAD_TIME` | Minimum notice | | | `1h` | `0s` disables the rule |
| `BOOKING_IDEMPOTENCY_TTL` | Replay window | | | `24h` | |
| `PAGE_SIZE_DEFAULT` | Default page | | | `20` | |
| `PAGE_SIZE_MAX` | Page ceiling | | | `100` | A larger `limit` is rejected, not clamped |

**Slot duration is not configurable.** It is fixed at 30 minutes by a `CHECK`
constraint, so an environment variable would offer a setting that silently breaks
every write. Changing it is a migration plus a code change.

### Security

| Variable | Purpose | Req | Sec | Default | Notes |
|---|---|:--:|:--:|---|---|
| `CORS_ALLOWED_ORIGINS` | Comma-separated allowlist | | | *(empty — CORS off)* | `*` is **rejected at startup** |
| `TRUST_PROXY_HEADERS` | Read `X-Forwarded-For` | | | `true` | Only safe behind a trusted proxy |

### Production safety checks

`config.Load` refuses to start production when:

- `PPROF_ENABLED=true` — profiles expose internal state and are unauthenticated
- `LOG_FORMAT=text` — logs must be machine-parseable
- `METRICS_ENABLED=true` with no `METRICS_AUTH_TOKEN` — it would be world-readable
- `LOG_LEVEL=debug` — request detail at scale
- `CORS_ALLOWED_ORIGINS` contains `*`

Failing to start is the correct behaviour. A service that boots with an
unauthenticated metrics endpoint is worse than one that refuses and says why.

### Connection budget

`DB_MAX_CONNS × replicas + headroom ≤ the database's max_connections`.

Headroom means migrations, a `psql` session during an incident, and any
monitoring agent. Railway's smaller PostgreSQL plans allow far fewer connections
than people assume — check the actual value:

```sql
SHOW max_connections;
SELECT count(*) FROM pg_stat_activity;
```

With `max_connections = 100`: two replicas at `DB_MAX_CONNS=10` uses 20, leaving
plenty. Ten replicas at 25 would be 250 and would fail — noisily, and at the
worst moment.

---

## Health endpoints

| Endpoint | Question | Checks the database? | Used by |
|---|---|:--:|---|
| `/livez` | Is the process alive? | **no** | Container health check |
| `/readyz` | Should traffic come here? | **yes**, 2s timeout | Railway, load balancers |

The split is not decoration. **A liveness probe that checks the database will
restart every replica during a database incident**, turning a degraded service
into no service and adding a thundering-herd reconnect on top. Liveness answers
only "can this process serve a handler?".

`/readyz` returns `503` when the database is unreachable **or** while the process
is draining after `SIGTERM`. Its body names each dependency and its state, and
never includes the connection string or the driver error — it is unauthenticated
and internet-facing. A test asserts that.

```json
{ "status": "ready",
  "checks": { "accepting_traffic": "ok", "database": "ok" },
  "version": "v1.0.0", "commit": "a1b2c3d" }
```

---

## Structured logs

JSON in deployed environments. Text is available locally, where a human is
reading a terminal; production refuses it.

### Fields on every request

| Field | Example | Notes |
|---|---|---|
| `time` | `2026-08-19T12:16:14.044Z` | |
| `level` | `INFO` | |
| `service`, `env` | `ratiba-api`, `production` | |
| `version`, `commit` | `v1.0.0`, `a1b2c3d` | Injected with `-ldflags` |
| `event` | `http.request` | Machine-filterable event type |
| `request_id` | `018f4e0a-…` | **The correlation key** |
| `trace_id`, `span_id` | | Present when tracing is on |
| `method` | `PATCH` | |
| `route` | `/appointments/{appointmentID}/cancel` | **The matched template, never the raw path** |
| `status` | `200` | |
| `duration_ms` | `3` | |
| `response_bytes` | `421` | |
| `user_agent` | truncated to 120 chars | |

### Cardinality

`route` is the chi route template. Logging the raw path would put an appointment
UUID in every line, making log cardinality unbounded and any group-by useless.
The same value is the metric label, for the same reason.

This is why the access log is written **after** the handler returns — that is the
first moment chi has resolved the template.

### Event types

| `event` | Meaning |
|---|---|
| `http.request` | A completed request |
| `http.rejected` | A 4xx, logged at info — normal traffic, not a fault |
| `http.error` | A 5xx, logged at error with the internal cause |
| `http.panic` | A recovered panic, with a stack trace |
| `appointment.booked` / `.cancelled` / `.rescheduled` | Domain events |
| `appointment.slot_conflict` | A booking lost a race |
| `health.not_ready` | A readiness check failed |

### Never logged

Cancellation reasons · patient names and emails · request or response bodies ·
`DATABASE_URL` or any password · `METRICS_AUTH_TOKEN` · `Idempotency-Key`
values · raw paths containing identifiers.

The configuration struct implements `slog.LogValue`, so a `Config` **cannot** be
logged raw — the redacted form is the only form. Enforced by
`TestSecretsAreNeverLogged` and `TestAccessLogOmitsSensitiveData`.

---

## Metrics

`GET /metrics`, Prometheus text format. Guarded by a bearer token when
`METRICS_AUTH_TOKEN` is set:

```bash
curl -H "Authorization: Bearer $METRICS_AUTH_TOKEN" https://your-app/metrics
```

### Catalogue

| Metric | Type | Labels | Use |
|---|---|---|---|
| `http_requests_total` | counter | `method`, `route`, `status_class` | Traffic and error rate |
| `http_request_duration_seconds` | histogram | `method`, `route`, `status_class` | Latency percentiles |
| `http_requests_in_flight` | gauge | — | Concurrency; saturation |
| `http_response_size_bytes` | histogram | `method`, `route` | Payload growth |
| `http_handler_panics_total` | counter | — | **Should always be zero** |
| `appointment_operations_total` | counter | `operation`, `outcome` | The domain signal |
| `db_pool_total_connections` | gauge | — | Pool size |
| `db_pool_acquired_connections` | gauge | — | In use |
| `db_pool_idle_connections` | gauge | — | Idle |
| `db_pool_max_connections` | gauge | — | Ceiling |
| `db_pool_acquire_total` | counter | — | Acquisitions |
| `db_pool_acquire_duration_seconds_total` | counter | — | **Rising rate ⇒ saturation** |
| `db_pool_empty_acquire_total` | counter | — | Waits for a free connection |
| `db_pool_canceled_acquire_total` | counter | — | Callers gave up |
| `ratiba_build_info` | gauge | `version`, `commit`, `build_time`, `go_version`, `env` | Always 1; the info is in the labels |

Plus the standard Go runtime and process collectors.

Every label comes from a small fixed vocabulary: `operation` ∈ {`book`,
`cancel`, `reschedule`}, `outcome` ∈ {`succeeded`, `replayed`, `conflict`,
`rejected`, `failed`}, `status_class` ∈ {`2xx`…`5xx`}. No label ever carries an
identifier — an unbounded label turns a metrics endpoint into a memory leak.

### Queries worth having

```promql
# Error rate
sum(rate(http_requests_total{status_class="5xx"}[5m]))
  / sum(rate(http_requests_total[5m]))

# p95 latency by route
histogram_quantile(0.95,
  sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))

# Booking conflict rate — contention, not necessarily a fault
sum(rate(appointment_operations_total{operation="book",outcome="conflict"}[5m]))
  / sum(rate(appointment_operations_total{operation="book"}[5m]))

# Pool saturation: seconds spent waiting per second of wall time
rate(db_pool_acquire_duration_seconds_total[5m])

# Pool utilisation
db_pool_acquired_connections / db_pool_max_connections
```

### Suggested alerts

Not configured — there is no alerting backend. Starting points:

| Alert | Condition | Severity |
|---|---|---|
| High error rate | 5xx ratio > 1% for 5m | page |
| Service unready | `/readyz` failing for 2m | page |
| Panic | `increase(http_handler_panics_total[5m]) > 0` | page — always a bug |
| Pool saturated | `db_pool_acquired / db_pool_max > 0.9` for 5m | ticket |
| Latency regression | p95 > 1s for 10m | ticket |
| Conflict spike | conflict ratio > 20% for 15m | ticket — see the [runbook](runbooks/booking-conflicts.md) |

### SLO starting point

99.5% of requests succeed (non-5xx) and 95% complete under 500 ms, measured over
30 days. Deliberately modest: an SLO nobody can meet is worse than none.

---

## Tracing

OpenTelemetry over inbound HTTP and PostgreSQL. Enable with:

```bash
OTEL_TRACES_ENABLED=true
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318
```

**Tracing degrades safely by design.** Disabled, or enabled with no endpoint,
installs a no-op provider. A collector that is unreachable at startup does **not**
fail startup — the exporter retries in the background and drops spans if it
cannot. An observability backend being down must never take the API down with it.

W3C trace context propagation is installed even when tracing is off, so an
inbound `traceparent` still reaches the logs.

Spans are named after the **matched route** (`POST /appointments`), renamed after
routing for the same cardinality reason as the logs. Health probes and metric
scrapes are filtered out — they would dominate trace volume and tell nobody
anything.

Local stack:

```bash
make observability
# Grafana     http://localhost:3000  (anonymous admin)
# Prometheus  http://localhost:9090
```

Datasources are provisioned from files, so it works with no clicking.

---

## Debugging a request

A user reports: *"I got an error booking an appointment this morning."*

### 1. Get the request ID

It is in the error body as `request_id` and in the `X-Request-Id` response
header. Every response has one, including successes.

### 2. Find every line for that request

```bash
# Railway
railway logs --environment production | grep 018f4e0a-1c2b-7d3e-9f01-2a3b4c5d6e7f

# Local
docker compose logs api --no-log-prefix | grep 018f4e0a-…
```

Every line from that request carries it — including domain events written deep in
the service, which never saw an `*http.Request`. That is the whole point of
carrying the ID in the context rather than threading it through signatures.

### 3. Read the story

```json
{"event":"http.rejected","error_code":"slot_unavailable","status":409,"request_id":"018f…"}
{"event":"appointment.slot_conflict","doctor_id":"7f3c…","starts_at":"2026-09-07T06:00:00Z","request_id":"018f…"}
{"event":"http.request","method":"POST","route":"/appointments","status":409,"duration_ms":12,"request_id":"018f…"}
```

Someone else took the slot. Working as designed.

### 4. If it was a 5xx

The `http.error` line carries the internal cause, which the client never saw:

```json
{"event":"http.error","error_code":"internal_error","status":500,
 "error":"internal_error (internal): …: book appointment: create appointment: ERROR: …"}
```

### 5. Pivot into the trace

If `trace_id` is present, open it in Grafana/Tempo to see the exact SQL and its
timing.

### 6. Confirm the state

```bash
curl "$API/appointments/$APPOINTMENT_ID"
curl "$API/patients/$PATIENT_ID/appointments"
```

Useful when a client timed out and cannot tell whether the write landed. If they
sent an `Idempotency-Key`, replaying the same request is safe and returns the
original outcome.

---

## Common tasks

### Deploy a specific commit

Merge to the branch, or use `workflow_dispatch` on the deploy workflow.

### Roll back

Redeploy the previous release from Railway's deployment history. **Do not roll
migrations back** — see [deployment](deployment.md#rollback).

### Inspect the database

```bash
# Local
make psql

# Railway
railway connect Postgres --environment production
```

```sql
-- Today's bookings for a doctor
SELECT starts_at, status FROM appointments
WHERE doctor_id = '…' AND starts_at::date = CURRENT_DATE
ORDER BY starts_at;

-- Audit trail for one appointment
SELECT event_type, from_starts_at, to_starts_at, source, occurred_at
FROM appointment_events WHERE appointment_id = '…' ORDER BY occurred_at;

-- Is anything blocked?
SELECT pid, state, wait_event_type, wait_event, left(query, 80)
FROM pg_stat_activity WHERE datname = current_database() AND state <> 'idle';
```

### Idempotency-key retention

```bash
railway run --environment production /usr/local/bin/ratiba-migrate purge-idempotency
```

Deliberately a manual command rather than a background goroutine: a scheduled job
is visible and has logs; a forgotten goroutine can silently stop. At this data
volume it is not yet needed.

### Re-seed demo data

```bash
ratiba-migrate seed
```

Idempotent, and never touches appointments — it will not destroy data a reviewer
just created.

---

## Backup and restore

**Not implemented.** Railway's managed PostgreSQL provides automated backups on
paid plans; this project neither configures nor tests them.

Before this handles real patient data, the following would be required, in this
order:

1. Automated daily backups with a defined retention period
2. **A tested restore.** An untested backup is not a backup
3. Point-in-time recovery for the accidental-`DELETE` case
4. A documented RPO and RTO
5. Encryption at rest and in transit for backup artifacts

Recording this as a gap rather than implying it is handled is the honest position.

---

## Runbooks

| Symptom | Runbook |
|---|---|
| API returning errors or not responding | [api-unhealthy.md](runbooks/api-unhealthy.md) |
| Database unavailable or pool exhausted | [database-unavailable.md](runbooks/database-unavailable.md) |
| More `409 slot_unavailable` than usual | [booking-conflicts.md](runbooks/booking-conflicts.md) |
| Elevated 5xx | [elevated-5xx.md](runbooks/elevated-5xx.md) |
| Requests slow | [slow-requests.md](runbooks/slow-requests.md) |
| A migration failed during deploy | [failed-migration.md](runbooks/failed-migration.md) |
| Correlating a customer report | [correlate-a-report.md](runbooks/correlate-a-report.md) |
