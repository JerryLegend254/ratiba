# Runbook — database unavailable or pool exhausted

## Detection

- `/readyz` reports `"database": "unavailable"`
- Logs contain `event=health.not_ready` with `dependency=database`
- 5xx responses with `error_code=internal_error`
- `db_pool_acquired_connections / db_pool_max_connections` near 1
- `rate(db_pool_acquire_duration_seconds_total[5m])` climbing

## Distinguish the two failures first

They look similar and have opposite fixes.

| | Database **down** | Pool **exhausted** |
|---|---|---|
| `/readyz` | fails immediately | fails slowly, or intermittently |
| Logs | `connection refused`, `no such host` | `context deadline exceeded` |
| `db_pool_acquired` | 0 or falling | at max |
| `db_pool_empty_acquire_total` | flat | rising fast |
| Effect | total outage | slow, partial degradation |

## Diagnosis

### Database down

```bash
railway status --environment production           # is the Postgres service up?
railway logs --service Postgres --environment production | tail -50
railway connect Postgres --environment production # can you connect at all?
```

Causes: the service is stopped or restarting, the plan's storage is full, or
credentials rotated without the app picking them up.

**Check `DATABASE_URL` is a reference, not a literal.** It should be
`${{Postgres.DATABASE_URL}}`. A hard-coded copy goes stale the moment the
password rotates — a genuinely common cause of a sudden, total, "nothing
changed" outage.

### Pool exhausted

```promql
db_pool_acquired_connections
db_pool_max_connections
rate(db_pool_empty_acquire_total[5m])
rate(db_pool_canceled_acquire_total[5m])
```

```sql
-- Who is holding connections?
SELECT state, count(*) FROM pg_stat_activity
WHERE datname = current_database() GROUP BY state;

-- Long-running or blocked queries
SELECT pid, state, wait_event_type, wait_event,
       now() - query_start AS duration, left(query, 100)
FROM pg_stat_activity
WHERE datname = current_database() AND state <> 'idle'
ORDER BY duration DESC LIMIT 10;

-- Is the server's own limit the ceiling?
SHOW max_connections;
SELECT count(*) FROM pg_stat_activity;
```

Causes, in order of likelihood:

1. **Total connections exceed the server limit.** `DB_MAX_CONNS × replicas` plus
   headroom must fit inside `max_connections`. See
   [operations](../operations.md#connection-budget).
2. **A long-running query** holding connections. `DB_STATEMENT_TIMEOUT` (8s)
   should cap this — if queries run longer, something is bypassing it.
3. **Traffic genuinely exceeds capacity.**
4. **A leaked connection.** The pool would show `acquired` at max with the
   database idle. The architecture forbids acquiring a second connection inside a
   transaction precisely to prevent this; if it appears, look for a new code path
   that broke that rule.

## Mitigation

### Immediate

```bash
# Kill a query that is blocking everything (targeted, not a blanket kill)
SELECT pg_cancel_backend(<pid>);   -- polite
SELECT pg_terminate_backend(<pid>); -- forceful

# Reduce pressure
railway variables --set DB_MAX_CONNS=5   # then redeploy
```

Reducing `DB_MAX_CONNS` is counter-intuitive but correct when the **server**
limit is the ceiling: fewer connections per replica means more replicas fit.

### Structural

| Situation | Action |
|---|---|
| Server limit reached | Lower `DB_MAX_CONNS`, or upgrade the database plan |
| Slow queries | Find them via `pg_stat_activity`, add an index, or lower `DB_STATEMENT_TIMEOUT` |
| Genuine load growth | Upgrade the plan; consider PgBouncer in transaction mode |
| Leaked connections | Find the code path holding a connection across an await |

## Escalation

Escalate if the database is unreachable and Railway shows the service healthy —
that suggests a platform networking issue.

## Verify recovery

```bash
curl -s "$API/readyz" | jq '.checks.database'   # "ok"
```

```promql
db_pool_acquired_connections / db_pool_max_connections   # comfortably below 1
rate(db_pool_empty_acquire_total[5m])                    # back to ~0
```

Then run a read-only smoke test: `bash scripts/smoke.sh "$API"`.
