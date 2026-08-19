# ADR 0001 — Modular monolith, not microservices

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

The brief describes one clinic with five doctors, one bounded context
(scheduling), and an explicit intention to grow. "We're starting small but want
to grow" is often read as an invitation to build for scale up front.

## Decision

Build a **single deployable service** with strong internal module boundaries:

- Domain packages (`appointment`, `doctor`, `patient`) contain no I/O and import
  no HTTP or SQL package.
- The domain declares the interfaces it needs; adapters implement them.
- Transport DTOs are separate types from domain models.
- `cmd/api` is the only place concrete implementations are chosen.

## Alternatives considered

### Microservices (booking, scheduling, notifications)

**Rejected.** The system's core invariant is *one unique index*. Splitting the
service across a network would replace a database constraint with a distributed
transaction — trading a guarantee that PostgreSQL provides for free against
sagas, compensating actions and eventual consistency. That is not a scaling
strategy; it is a correctness downgrade.

It would also add per-service pipelines, inter-service auth, distributed tracing
as a necessity rather than a nicety, and a local development story requiring
several processes — all to serve five doctors.

### A layered "clean architecture" with an interface per type

**Rejected as ceremony.** Interfaces are defined where there is a genuine seam:
persistence and time. A `DoctorService` interface with exactly one
implementation, existing so a mock can be generated, adds indirection without
adding a boundary.

### A single package

**Rejected.** The brief explicitly asks for sensible structure, and more
importantly the domain must be testable without a database. That requires the
domain not to import the adapter, which requires packages.

## Consequences

### Good

- One binary, one deploy, one place to look during an incident.
- Transactions are local. Cancel-and-audit is `BEGIN … COMMIT`, not a saga.
- The domain tests run in milliseconds with no infrastructure.
- Extraction later is mechanical: the seams already exist. `internal/postgres`
  could be swapped wholesale without touching a domain file.

### Bad

- Everything scales together. Acceptable — the workload is uniform, and the
  service is stateless so horizontal scaling works.
- Module boundaries are a convention the compiler only partly enforces. The
  import rule ("domain packages import no I/O") has to be held by review until
  it is written down somewhere a contributor will read.

### Neutral

- If a genuinely separate context appears (billing, notifications), it can become
  its own service. That decision would get its own ADR.
