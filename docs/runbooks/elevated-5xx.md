# Runbook — elevated 5xx responses

## Detection

```promql
sum(rate(http_requests_total{status_class="5xx"}[5m]))
  / sum(rate(http_requests_total[5m]))
```

Log lines with `event=http.error`, or `event=http.panic`.

## Check for panics first

```promql
increase(http_handler_panics_total[15m])
```

**A panic is always a bug.** If this is non-zero, go straight to
[Panics](#panics) below.

## Likely causes

1. A panic in a handler (a bug)
2. The database is unreachable or the pool is exhausted
3. Query timeouts under load
4. An unhandled database error that is not being translated
5. A bad deploy

## Diagnosis

### 1. Which route, and which error code?

```promql
sum by (route) (rate(http_requests_total{status_class="5xx"}[5m]))
```

One route → a specific code path. All routes → infrastructure.

### 2. Read the internal cause

Unlike the client, the log has it:

```bash
railway logs --environment production | grep 'event=http.error' | tail -20
```

```json
{"event":"http.error","error_code":"internal_error","status":500,
 "error":"internal_error (internal): …: book appointment: create appointment: ERROR: …",
 "request_id":"018f…"}
```

The wrapped chain reads outside-in and usually names the failure precisely.

### 3. Did it start with a deploy?

```bash
curl -s "$API/" | jq '.commit'
railway deployments --environment production | head
```

Correlate the onset with a deployment time. If they match, **roll back first and
diagnose after.**

### 4. Is it the database?

```bash
curl -s "$API/readyz" | jq
```

If `"database": "unavailable"`, go to
[database-unavailable.md](database-unavailable.md).

## Panics

```bash
railway logs --environment production | grep 'event=http.panic' -A 30
```

The log carries the panic value, the full stack trace, the method, the matched
route and the request ID. The handler already returned a safe `500` — no data was
exposed.

1. Note the route and stack.
2. **Roll back** if it started with a deploy.
3. Reproduce in a test. A panic is a nil dereference or an out-of-range index, and
   should be a one-line fix plus a regression test.

`http_handler_panics_total` should be permanently zero. Alert on any increase.

## Mitigation

| Cause | Action |
|---|---|
| Bad deploy | **Roll back from Railway's deployment history.** Fastest restoration |
| Database down | [database-unavailable.md](database-unavailable.md) |
| Pool exhausted | [database-unavailable.md](database-unavailable.md) |
| Query timeouts | [slow-requests.md](slow-requests.md) |
| Panic | Roll back, then fix forward with a test |

## Escalation

Escalate if 5xx persists after a rollback and `/readyz` reports the database
healthy — that suggests something outside both the application and the database.

## Verify recovery

```promql
sum(rate(http_requests_total{status_class="5xx"}[5m])) # ~0
increase(http_handler_panics_total[15m])               # 0
```

```bash
bash scripts/smoke.sh "$API"
```
