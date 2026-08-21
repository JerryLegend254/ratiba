# Contributing to Ratiba

This document takes you from a fresh clone to a merged change. If anything here
is wrong or missing, fixing it is a welcome first contribution — stale setup
instructions are the most expensive kind of stale documentation.

---

## Prerequisites

| Tool | Needed for | Notes |
|---|---|---|
| **Docker** + Compose | Running the stack | The only hard requirement to run the app |
| **Go** (version in [`go.mod`](go.mod)) | Building, testing, native runs | |
| `sqlc` | `make generate` | Installed by `make bootstrap` |
| `golangci-lint` | `make lint` | Installed by `make bootstrap` |
| `govulncheck` | `make vulncheck` | Installed by `make bootstrap` |
| `psql` | `make psql` | Optional convenience |

Run `make doctor` at any point. It checks each of these and tells you exactly
what to do about anything missing, rather than letting you discover it one
confusing error at a time.

---

## Setup from a fresh clone

```bash
git clone <repository-url>
cd ratiba

make doctor      # check prerequisites
make bootstrap   # install pinned tools, create .env, download modules
make dev         # build and start everything, wait until it answers
```

`make dev` starts PostgreSQL, applies migrations, seeds demo data, starts the
API, and polls `/readyz` until it responds. When it prints the URLs, the system
is live.

Verify:

```bash
make smoke       # read-only checks against the running API
```

### Running natively, without Docker

Useful when you want a debugger attached or a fast edit-run loop.

```bash
docker compose up -d postgres    # you still want a real database
make migrate-up
make seed
make run
```

`make run` reads `.env` through your shell. The defaults in `.env.example` point
at the compose PostgreSQL on `localhost:5432`.

---

## The development loop

```bash
# 1. Make a change.

# 2. Fast feedback — no database, a couple of seconds.
make unit-test

# 3. If you touched SQL, the schema, or the persistence layer:
make integration-test

# 4. Before opening a pull request:
make check       # formatting, vet, lint, generated-code drift, contract, links
make test        # both suites
```

`make ci` runs everything the pipeline runs. If it is green locally, CI will be
green.

### Where new code goes

| If you are adding… | It belongs in… |
|---|---|
| A booking rule (when a slot is valid) | `internal/appointment/policy.go` |
| A use case (a new operation on appointments) | `internal/appointment/service.go` |
| A new HTTP endpoint | `internal/transport/http/handlers_*.go` + `api/openapi.yaml` |
| A new query | `db/queries/*.sql`, then `make generate` |
| A schema change | A new file in `db/migrations/` |
| A new configuration option | `internal/platform/config/config.go` + `.env.example` + `docs/operations.md` |
| A new error code | `internal/platform/apperror/apperror.go` + the catalogue in `handlers_meta.go` |
| Cross-cutting infrastructure | `internal/platform/…` |

**The rule that matters:** business logic goes in the domain packages
(`appointment`, `doctor`, `patient`), which must not import `net/http`, `pgx`, or
anything else with I/O. If you find yourself needing a database handle inside
`policy.go`, the design is being violated — see
[docs/architecture.md](docs/architecture.md).

---

## Code generation

SQL is written by hand in `db/queries/` and compiled into type-safe Go by
[sqlc](https://sqlc.dev). Generated code is **committed**, and CI fails if it
differs from a fresh run.

```bash
# 1. Edit or add a query in db/queries/*.sql
# 2. Regenerate
make generate
# 3. Commit both the .sql file and the generated Go
```

`sqlc` reads the schema directly from `db/migrations/`, so generated code cannot
drift from what is actually deployed.

Two gotchas learned the hard way:

- **Do not use the same positional parameter twice with different casts.**
  PostgreSQL cannot deduce a type (`SQLSTATE 42P08`) and sqlc names the argument
  `Column3`. Pass the values separately instead.
- **Cast expressions lose the column name.** If a generated parameter is called
  `Column2`, use a named parameter (`@starts_at`) or restructure the query.

---

## Database migrations

```bash
# Create a new migration. The number must be the next one in sequence.
$EDITOR db/migrations/00004_add_something.sql

make migrate-up       # apply
make migrate-status   # see what is applied
make migrate-down     # roll back the most recent (local only)
```

### Rules

1. **Every migration needs a working `-- +goose Down`.** CI runs
   `TestMigrationsRollBackAndReapply`, which migrates all the way down and back
   up against a throwaway database. A broken `Down` fails there, not during an
   incident.
2. **Numbering is strictly sequential**, no gaps, no reuse. `make
   verify-migrations` enforces it — goose applies in filename order, so a gap
   means two environments can end up with different schemas from one commit.
3. **Wrap statements containing semicolons** (PL/pgSQL function bodies) in
   `-- +goose StatementBegin` / `-- +goose StatementEnd`.
4. **Migrations must be forward-compatible with the running code.** During a
   deploy the old version is still serving while the migration runs. Adding a
   `NOT NULL` column with no default breaks it. Use the expand/contract pattern:
   add nullable, backfill, make it required in a *later* release.
5. **Never edit an applied migration.** Write a new one.
6. Run `make generate` afterwards — sqlc reads the migrations.

---

## Testing expectations

Full detail: [docs/testing.md](docs/testing.md).

| Change | Minimum expected tests |
|---|---|
| A booking rule | A table-driven case in `policy_test.go`, both the accepted and the rejected side |
| A use case | A `service_test.go` case using the in-memory store |
| An endpoint | A `server_test.go` case asserting status, body shape and the error code |
| A query or schema change | An integration test in `internal/postgres/` |
| Anything touching concurrency | An integration test that actually runs concurrent goroutines |

Guidelines:

- **Test behaviour, not implementation.** Assert on the returned status and
  error code, not on which internal function was called.
- **Never call `time.Now()` in a test.** Inject `clock.Fixed`. Tests that depend
  on wall-clock time fail differently depending on when CI runs.
- **Table-driven where there is more than one case**, with a `name` that reads
  as a sentence: `"slot starting exactly at closing time does not fit"`.
- **The in-memory store cannot prove concurrency.** A mutex-guarded map proves
  nothing about PostgreSQL. Concurrency claims belong in
  `internal/postgres/concurrency_test.go`.

---

## Branch and pull-request workflow

```
feature branch  →  PR into dev  →  dev → staging  →  staging → main
```

| Branch | Deploys to | Protection |
|---|---|---|
| `dev` | development | PR + CI required |
| `staging` | staging | PR + CI required |
| `main` | production | PR + CI required, reviewer recommended |

Branch from `dev`. Name branches `feat/…`, `fix/…`, `docs/…` or `chore/…`.

Merging a pull request produces a `push` to the target branch, and that push
triggers deployment. There is no separate "on merge" event.

### Commit messages

[Conventional Commits](https://www.conventionalcommits.org/):

```
feat(appointment): reject reschedule to the current slot

Returning 200 would append a 'rescheduled' audit event describing a move
that never happened. A 409 with a stable code lets clients distinguish it
from a genuine slot conflict.

Refs: docs/adr/0006-reschedule-semantics.md
```

Explain **why**, not what — the diff already shows what.

### Pull request checklist

- [ ] `make check` passes
- [ ] `make test` passes
- [ ] New behaviour has tests; a bug fix has a test that fails without it
- [ ] `api/openapi.yaml` updated if the wire format changed (CI checks both directions)
- [ ] `.env.example` and `docs/operations.md` updated if configuration changed
- [ ] An ADR added if the decision would otherwise need explaining twice
- [ ] No secrets, no patient data in logs, no `TODO` left behind
- [ ] Documentation updated if a documented command or behaviour changed

---

## Code style

Beyond `gofmt` and the linters, three conventions this codebase holds to:

**Comments explain why, never what.**

```go
// Bad — restates the code
// Loop over the windows
for _, window := range windows {

// Good — explains a non-obvious decision
// Loading the schedule before opening the transaction is deliberate: acquiring
// a second pool connection while holding one is how a pool deadlocks itself.
```

**Errors are wrapped with context, and the client never sees the cause.**

```go
return apperror.New(apperror.KindConflict, apperror.CodeSlotUnavailable,
    "That slot is no longer available.").WithCause(err)
```

The `Message` is safe for an unauthenticated caller; the cause goes only to logs.

**Exported symbols have doc comments.** Someone will read this package who has
never met you. `revive` enforces it.

---

## Getting help

- Architecture questions → [docs/architecture.md](docs/architecture.md), then [docs/adr/](docs/adr/)
- "Why does this fail?" → [docs/testing.md](docs/testing.md#troubleshooting)
- "Why is it behaving like that in production?" → [docs/runbooks/](docs/runbooks/)
- Unfamiliar term → [docs/glossary.md](docs/glossary.md)
