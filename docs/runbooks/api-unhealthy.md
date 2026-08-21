# Runbook — the API is unhealthy

## Detection

- `/readyz` returns non-200, or does not respond
- Railway shows the deployment as unhealthy or crash-looping
- Clients report timeouts or connection errors

## Likely causes, most common first

1. Configuration validation failed at startup — the process refuses to boot
2. The database is unreachable
3. The deploy never completed (the pre-deploy migration failed)
4. The process is crash-looping
5. Resource exhaustion (memory)

## Diagnosis

### 1. Is the process running at all?

```bash
railway status --environment production
railway logs --environment production | tail -100
```

**A configuration failure is unmistakable** and is the first thing to rule out:

```
fatal: invalid configuration:
  - METRICS_AUTH_TOKEN: is required in production when METRICS_ENABLED is true…
  - LOG_FORMAT: must be "json" in production…
```

The message names every problem at once. Fix the variables and redeploy. This is
fail-closed behaviour working as intended, not a fault.

### 2. Is it alive but not ready?

```bash
curl -s "$API/livez"    # 200 = the process is fine
curl -s "$API/readyz"   # 503 = a dependency is not
```

`livez` 200 and `readyz` 503 means **the process is healthy and a dependency is
not.** Go to [database-unavailable.md](database-unavailable.md).

The `readyz` body names the failing check:

```json
{"status":"not_ready","checks":{"accepting_traffic":"ok","database":"unavailable"}}
```

If `accepting_traffic` is `draining`, the instance is shutting down — normal
during a deploy, a problem if it persists.

### 3. Did the deploy complete?

Check the Railway deployment log for the pre-deploy step. If the migration
failed, the release was aborted and the **previous version should still be
serving**. Go to [failed-migration.md](failed-migration.md).

### 4. Is it crash-looping?

Repeated `starting ratiba` lines with no `http server listening` between them.
The line immediately before each restart is the cause.

`restartPolicyMaxRetries` is 5, so a deterministic startup failure surfaces as a
failed deploy rather than an endless loop.

## Mitigation

| Cause | Action |
|---|---|
| Bad configuration | Fix the variable in Railway and redeploy. The error names it exactly |
| Database unreachable | [database-unavailable.md](database-unavailable.md) |
| Bad release | **Redeploy the previous release from Railway's deployment history.** Fastest path back |
| Migration failed | [failed-migration.md](failed-migration.md) |
| Out of memory | Increase the plan's memory, or reduce `DB_MAX_CONNS` |

**Prefer rolling the application back over debugging live.** Restore service
first, diagnose after.

## Escalation

Escalate if the previous release also fails to become healthy — that points at
the database or the platform rather than the application.

## Verify recovery

```bash
curl -s "$API/readyz" | jq          # {"status":"ready", …}
bash scripts/smoke.sh "$API"        # read-only, safe in production
curl -s "$API/" | jq '.commit'      # is the expected version serving?
```

Then confirm the error rate has returned to baseline:

```promql
sum(rate(http_requests_total{status_class="5xx"}[5m]))
```
