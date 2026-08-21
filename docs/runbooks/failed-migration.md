# Runbook — a migration failed during deploy

## Detection

- The Railway deployment log shows the pre-deploy step failing
- The GitHub Actions deploy job fails at the `railway up` step
- The deployment is marked failed and never rolled out

## Read this first — you are probably not down

`preDeployCommand` runs **before** any new replica starts. If it fails, Railway
**aborts the release** and the previous version keeps serving.

So the usual situation is: **the deploy failed, the service is fine.** Confirm
that before doing anything else:

```bash
curl -s "$API/readyz" | jq
curl -s "$API/" | jq '.commit'    # should be the PREVIOUS commit
```

If that returns healthy, you have time. Do not rush into a database change under
pressure.

## Likely causes

1. A SQL error in the new migration (syntax, or a constraint that existing data
   violates)
2. A missing extension — `btree_gist` is required by migration `00001`
3. A lock timeout: the migration needed a lock a live query was holding
4. The advisory lock is held by another migration process
5. Connectivity between the pre-deploy container and the database

## Diagnosis

### 1. Read the actual error

```bash
railway logs --environment production | grep -A 20 'goose\|migrate'
```

goose output is structured; the failure names the migration and the SQL error:

```json
{"level":"ERROR","msg":"goose up: ERROR: column \"foo\" contains null values (SQLSTATE 23502)"}
```

### 2. What state is the schema in?

**This is the critical question.** goose runs each migration in a transaction, so
a failed migration is rolled back and the schema is left at the previous version
— not half-applied.

```bash
railway run --environment production /usr/local/bin/ratiba-migrate status
```

```
Applied At                  Migration
=======================================
Mon Aug 19 10:00:00 2026 -- 00001_core_schema.sql
Mon Aug 19 10:00:00 2026 -- 00002_appointments.sql
Pending                  -- 00003_idempotency_keys.sql
```

`Pending` for the failing migration is the expected, healthy outcome.

### 3. Is the advisory lock stuck?

```sql
SELECT pid, locktype, objid, granted FROM pg_locks WHERE locktype = 'advisory';
```

The lock is session-scoped, so it is released when the connection closes — even
if the process was killed. A lingering lock means a connection is still open.

### 4. Is it a data problem rather than a schema problem?

The most common real cause: the migration is valid but existing data violates the
new constraint. For example adding `NOT NULL` where nulls exist.

```sql
-- Find the offending rows before changing anything
SELECT count(*) FROM appointments WHERE some_column IS NULL;
```

## Mitigation

### Do not roll the migration back

The service is on the previous version and the schema is at the previous version.
**They match.** Running `down` now would move the schema *behind* the running
code and turn a failed deploy into an outage.

### Fix forward

1. Reproduce locally against a copy of the shape of the production data:

   ```bash
   make up
   make migrate-up      # confirm it fails the same way
   ```

2. Fix the migration. If it was **never successfully applied anywhere**, editing
   it is acceptable. If it applied in development or staging, write a **new**
   migration instead — editing an applied migration means environments silently
   diverge.

3. For a data problem, use the expand/contract pattern:

   | Release | Action |
   |---|---|
   | 1 | Add the column **nullable** |
   | 2 | Backfill existing rows |
   | 3 | Add `NOT NULL` |

4. Push through the normal pipeline. `TestMigrationsRollBackAndReapply` will
   exercise it against a throwaway database in CI first.

### Missing extension

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;
```

Requires elevated privileges. On Railway the default database user has them. If
the platform forbids the extension, the working-hours exclusion constraint must
be replaced with a trigger — a schema change, not an operational fix.

## Escalation

Escalate if `ratiba-migrate status` shows a state that does not match either the
before or after version. That would mean a migration partially applied, which
should be impossible given transactional DDL — and would need careful manual
reconciliation.

## Verify recovery

```bash
railway run --environment production /usr/local/bin/ratiba-migrate status   # no Pending
curl -s "$API/readyz" | jq
curl -s "$API/" | jq '.commit'     # the NEW commit
bash scripts/smoke.sh "$API"
```

## Prevention

Already in place:

- `make verify-migrations` — numbering, ordering, and that both `Up` and `Down`
  sections exist
- `TestMigrationsRollBackAndReapply` — migrates fully down and back up in CI
- The CI integration job applies migrations to a clean database on every run
- Deploying through `dev` → `staging` → `main` means a bad migration fails in
  development first
