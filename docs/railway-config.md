# Railway configuration reference

Annotations for [`railway.json`](../railway.json). Railway's config format does
not allow comments, so the reasoning lives here.

## `build`

| Key | Value | Why |
|---|---|---|
| `builder` | `DOCKERFILE` | Railway's Nixpacks autodetection would build its own image with its own Go version and its own base layer. Using the committed `Dockerfile` means the artifact Railway runs is byte-for-byte the one CI built and tested. |
| `dockerfilePath` | `Dockerfile` | Explicit, so adding another Dockerfile later cannot silently change what is deployed. |

## `deploy`

| Key | Value | Why |
|---|---|---|
| `startCommand` | `/usr/local/bin/ratiba-api` | Absolute path, exec'd directly. There is no shell in the distroless image to interpret anything else, and PID 1 must be the binary so it receives `SIGTERM` itself. |
| `preDeployCommand` | `/usr/local/bin/ratiba-migrate up` | **The most important line in this file.** Migrations run once, in a separate container, from the same image, *before* any new replica starts. If it fails, the release is aborted and the previous version keeps serving. This is what stops N replicas racing to migrate on startup. |
| `healthcheckPath` | `/readyz` | Readiness, not liveness: a replica that cannot reach PostgreSQL must not receive traffic. `/livez` would report healthy in that state. |
| `healthcheckTimeout` | `120` seconds | Generous enough for a cold start plus pool warm-up on a small instance, short enough that a genuinely broken release fails the deploy rather than hanging it. |
| `overlapSeconds` | `20` | The old replica keeps serving while the new one becomes healthy, so a deploy drops no requests. |
| `drainingSeconds` | `20` | Matches `HTTP_SHUTDOWN_TIMEOUT`. The application fails readiness first, pauses, then drains in-flight requests; Railway must allow at least as long or it would `SIGKILL` mid-request. |
| `restartPolicyType` | `ON_FAILURE` | A crash is retried. `ALWAYS` would also restart a clean exit, which for this service only happens during shutdown. |
| `restartPolicyMaxRetries` | `5` | Bounded, so a configuration error (which fails deterministically at startup) surfaces as a failed deploy instead of an endless crash loop. |
| `numReplicas` | `1` | Honest default for an assessment deployment. Correctness does not depend on it: the partial unique index enforces the no-double-booking rule across any number of replicas, and the concurrency tests prove it. Raising this requires only that `DB_MAX_CONNS × replicas` stays inside the database's connection limit. |

## Environment variables per environment

`railway.json` is committed and identical across environments. Everything that
differs is a Railway variable, so no secret is ever in the repository:
`APP_ENV`, `DATABASE_URL` (as a `${{Postgres.DATABASE_URL}}` reference, never a
literal), `METRICS_AUTH_TOKEN`, and the pool and logging settings. `PORT` is
injected by the platform and must not be set.

## What deliberately is not here

- **No `watchPatterns`.** Every deploy is triggered by CI after the quality
  gates pass, not by Railway watching the repository. Two independent deploy
  triggers would race.
- **No cron / scheduled job.** The idempotency-key sweep
  (`ratiba-migrate purge-idempotency`) is deliberately not scheduled: at this
  data volume it is not yet needed, and an unattended job nobody monitors is a
  liability.
