# Runbooks

Symptom-first operational procedures. Each starts from **what you observed**, not
from what is wrong — because during an incident that is all you have.

| Symptom | Runbook |
|---|---|
| The API is down, or `/readyz` is failing | [api-unhealthy.md](api-unhealthy.md) |
| Database errors, or the pool is exhausted | [database-unavailable.md](database-unavailable.md) |
| More `409 slot_unavailable` than usual | [booking-conflicts.md](booking-conflicts.md) |
| Elevated 5xx | [elevated-5xx.md](elevated-5xx.md) |
| Requests are slow | [slow-requests.md](slow-requests.md) |
| A deploy failed during the migration step | [failed-migration.md](failed-migration.md) |
| A user reported a problem and you have their error | [correlate-a-report.md](correlate-a-report.md) |

Each runbook has the same shape: **detection · likely causes · diagnosis ·
mitigation · escalation · verify recovery**.

## Before you start

```bash
export API=https://<your-deployment>
curl -s "$API/readyz" | jq          # is it ready, and is the database reachable?
curl -s "$API/" | jq                # which version and commit is running?
railway logs --environment production | tail -50
```

## The two facts worth knowing first

1. **`/livez` deliberately does not check the database.** A process that is alive
   but cannot reach PostgreSQL will report `livez` healthy and `readyz` unhealthy.
   That is correct — see [operations](../operations.md#health-endpoints).
2. **Rolling a migration back in production is not the recovery path.** Roll the
   *application* back and fix forward. See
   [deployment](../deployment.md#rollback).
