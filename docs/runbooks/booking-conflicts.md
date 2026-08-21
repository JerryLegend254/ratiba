# Runbook — increased booking conflicts

## Detection

```promql
sum(rate(appointment_operations_total{operation="book",outcome="conflict"}[5m]))
  / sum(rate(appointment_operations_total{operation="book"}[5m]))
```

Also visible as a rise in `409` responses on `POST /appointments`, and log lines
with `event=appointment.slot_conflict`.

## Read this first

**A conflict is not an error.** `409 slot_unavailable` means the system worked:
two people wanted one slot, one got it, and the other was told the truth
immediately. The alternative — both succeeding — is the failure this entire
design exists to prevent.

The question is never "why are there conflicts?" but **"is this rate
explainable?"**

## Likely causes, most benign first

1. **Genuine contention.** A popular doctor, a newly-opened schedule, a campaign.
   Expected.
2. **A client not refetching availability.** It caches slots and keeps retrying
   dead ones.
3. **A retry storm.** A client treating `409` as retryable and hammering the same
   slot.
4. **Stale availability**, from an aggressive client-side cache or a CDN caching
   a response that is explicitly `no-store`.
5. **A bug in a client** constructing slot times itself rather than using the
   availability endpoint.

## Diagnosis

### 1. Is it concentrated or spread?

```sql
-- Which doctors and slots are being contended?
SELECT doctor_id, starts_at, count(*)
FROM appointments
WHERE status = 'cancelled' AND created_at > now() - interval '1 hour'
GROUP BY doctor_id, starts_at
HAVING count(*) > 1
ORDER BY count(*) DESC LIMIT 20;
```

Concentrated on one doctor or a few slots → genuine contention. Spread evenly
across every doctor → suspect a client.

### 2. Is the conflict rate proportional to traffic?

```promql
sum(rate(appointment_operations_total{operation="book"}[5m]))
```

A conflict rate rising **while total attempts stay flat** points at retries, not
demand.

### 3. Is one client responsible?

```bash
railway logs --environment production \
  | grep 'appointment.slot_conflict' | tail -50
```

Then correlate on `user_agent` in the matching `http.request` lines.

### 4. Confirm the invariant still holds

The reassuring check — there should be **no** rows returned:

```sql
SELECT doctor_id, starts_at, count(*)
FROM appointments WHERE status = 'booked'
GROUP BY doctor_id, starts_at HAVING count(*) > 1;
```

If this ever returns a row, the partial unique index is missing or was dropped.
That is a **severity-one data integrity incident**:

```sql
SELECT indexname, indexdef FROM pg_indexes
WHERE tablename = 'appointments' AND indexname = 'appointments_active_slot_uniq';
```

## Mitigation

| Cause | Action |
|---|---|
| Genuine contention | None needed. Consider client UX: refetch and re-render on `409` |
| Client not refetching | Contact the client owner. `docs/api.md` documents the expected behaviour |
| Retry storm | Rate-limit at the edge as an interim measure; the real fix is client-side backoff |
| Stale caching | Responses already send `Cache-Control: no-store`; check for an intermediary overriding it |
| Client constructing slots | Point them at `GET /doctors/{id}/availability` — its output is bookable by construction |

**Do not "fix" this by weakening the constraint.** The conflicts are the system
telling the truth.

## Escalation

Escalate immediately if the duplicate-check query above returns any row. That is
not a contention problem; it is a data integrity failure and the index must be
investigated at once.

## Verify recovery

```promql
sum(rate(appointment_operations_total{operation="book",outcome="conflict"}[5m]))
  / sum(rate(appointment_operations_total{operation="book"}[5m]))
```

Back to baseline. And the duplicate query still returns nothing.
