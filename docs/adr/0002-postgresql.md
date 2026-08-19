# ADR 0002 — PostgreSQL as the system of record

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

The brief says "use any database you prefer". The dominant requirement is
[ADR 0003](0003-concurrency-strategy.md): the no-double-booking invariant must be
enforced atomically, across replicas, by something other than application code.

## Decision

**PostgreSQL 17**, accessed with `pgx/v5` and `pgxpool`, with queries written by
hand in `db/queries/` and compiled by **sqlc** into type-safe Go.

Migrations use **goose**, embedded into the migrate binary with `embed.FS`.

## Alternatives considered

### MySQL / MariaDB

**Rejected.** No partial (filtered) indexes. The invariant would need either a
full unique index — which would prevent a slot from ever being rebooked after
cancellation, because cancelled rows would still occupy it — or a nullable
"active slot" column carrying the semantics implicitly. Both are worse. No
exclusion constraints either, closing the door on variable durations later.

### SQLite

**Rejected.** Single-writer. A file-backed database contradicts a stateless,
horizontally-scalable service.

### MongoDB or another document store

**Rejected.** The invariant is a multi-document uniqueness constraint, which is
precisely what document stores do not provide cheaply. A unique index on a
compound key exists, but partial-index semantics and transactional audit writes
are far more awkward.

### An ORM (GORM, ent)

**Rejected.** The queries here are few and simple, and the hard parts —
`SELECT … FOR UPDATE`, a partial unique index, an `int4range` generated column,
a GiST exclusion constraint — are where ORMs are least helpful and most likely to
generate something subtly different from what was intended.

sqlc inverts the trade-off: the SQL is written explicitly and reviewed, and the
*Go* is generated. That is the right way round for a system whose correctness
lives in the schema.

### `database/sql` with hand-written scanning

**Rejected.** All the explicitness of sqlc with none of the type safety, plus
scanning boilerplate that is easy to get wrong when a column is added.

### golang-migrate instead of goose

Close call. **goose** was chosen because it embeds cleanly into a binary and its
`StatementBegin`/`StatementEnd` markers handle PL/pgSQL function bodies without
fighting the parser.

## Consequences

### Good

- Partial unique indexes, exclusion constraints, `timestamptz`, `int4range`,
  generated columns, enums — the schema can carry real invariants.
- `sqlc` reads the migrations directly, so generated code cannot drift from the
  deployed schema.
- Compile-time errors when a column changes, rather than a runtime scan failure.
- Managed PostgreSQL is available on every platform, Railway included.

### Bad

- sqlc is another tool in the chain, with its own generation step to keep in
  sync. Expected cost; revisit here if it proves sharper than anticipated.
- `btree_gist` is required for the working-hours exclusion constraint. Standard
  contrib, available on Railway, but a PostgreSQL without it fails the first
  migration — loudly, at pre-deploy, which is the right place to find out.
- Generated code is committed, so a regeneration shows in diffs. Worth it: CI can
  then verify it is current.

### Neutral

- Pinned to 17.5 in compose and CI so local and pipeline behaviour agree.
