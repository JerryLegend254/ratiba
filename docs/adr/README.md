# Architecture decision records

Short records of decisions whose *reasoning* would otherwise have to be
re-derived — or re-argued — every time someone new reads the code.

A decision belongs here when the "why" is not obvious from the code and a
reasonable engineer might have chosen differently. Decisions that are simply
correct do not need a record.

| # | Decision | Status |
|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith, not microservices | Accepted |
| [0002](0002-postgresql.md) | PostgreSQL as the system of record | Accepted |
| [0003](0003-concurrency-strategy.md) | A partial unique index enforces the booking invariant | Accepted |

Format: context, decision, alternatives, consequences, date. Records are
immutable — a reversed decision gets a **new** record that supersedes the old
one, because the history of what was believed and when is the useful part.
