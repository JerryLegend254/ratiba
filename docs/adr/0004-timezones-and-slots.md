# ADR 0004 — Store instants, interpret in the doctor's timezone

- **Status:** Accepted
- **Date:** 2026-08-19

## Context

"Each doctor has set working hours and works in 30-minute slots" hides three
distinct concepts that are easy to conflate:

1. **An instant** — when the appointment actually happens.
2. **A wall-clock time** — "Dr. Wanjiru starts at 09:00". This is *not* an
   instant; it is a rule that produces different instants on different days.
3. **A calendar date** — "show me Monday's availability". Also not an instant,
   and its meaning depends on whose Monday.

Getting these wrong produces bugs that appear twice a year, in production, for
some users only.

The clinic is in Nairobi (UTC+3, no DST), but the brief says the clinic wants to
grow, and a design that only works in a DST-free zone is a design with a fuse in
it.

## Decision

### Storage

| Concept | Stored as |
|---|---|
| Appointment start and end | `timestamptz` — a UTC instant |
| Working hours | `time` (wall clock) + the doctor's IANA zone |
| Requested calendar date | Not stored — a query parameter |

### Interpretation

**The doctor's IANA timezone is the sole authority.** The host machine's zone is
never consulted; `clock.System` returns UTC and every conversion is explicit.

The zone database is compiled into the binaries (`import _ "time/tzdata"`), so
`Africa/Nairobi` resolves inside a distroless container with no OS `tzdata`.

### Alignment is defined by generation, not by a check

A start time is aligned **precisely when it appears in the list the slot
generator produces** for that doctor and date. There is no separate
`minute % 30 == 0` check anywhere.

The generator:

1. Turns each working-hours interval into an absolute `[start, end)` window using
   the doctor's zone.
2. Steps through it in **absolute time**, so every slot is exactly 30 real
   minutes.
3. Emits a slot only if it fits **entirely** within the window.

Booking validates by membership in that list. Availability returns the same list
minus what is taken and what is inside the lead time.

## Alternatives considered

### Store a UTC offset instead of a zone name

**Rejected.** `+03:00` is correct for Nairobi and wrong for London for half the
year. An offset is a *result* of a zone and a date, not a substitute for one.

### Store working hours as UTC instants

**Rejected.** "09:00 local" would have to be recomputed and rewritten twice a
year for every DST zone. Wall-clock plus a zone is the stable representation —
the doctor's contract says 09:00, not 06:00Z.

### Do everything in UTC and ignore local time

**Rejected.** Availability is requested for a *local* calendar day. Interpreting
`?date=2026-09-07` as a UTC day would return the wrong slots for any doctor not
in UTC — silently, and only near midnight.

### Check alignment with a modulo instead of by membership

**Rejected**, and this is the subtle one. A separate check must agree with the
generator in every case, including across a DST transition where local wall-clock
alignment and 30-real-minute stepping diverge. Two rules that must agree
eventually disagree.

Deriving alignment from generation makes disagreement **impossible**: there is
one list, and it is the answer to both "what can I book?" and "is this
bookable?".

### Step in local wall-clock time rather than absolute time

**Rejected.** A 30-minute appointment must be 30 real minutes. Stepping the wall
clock across a spring-forward would produce a "30-minute" slot lasting 90 real
minutes.

## Consequences

### Good

- Availability and booking **cannot** disagree — proven by a test that feeds every
  offered slot back through the validator.
- DST is handled correctly without special-casing: local wall clock stays fixed,
  the UTC instant shifts, and a window a transition passes through simply yields
  fewer slots because fewer whole slots fit.
- Multi-timezone doctors work today. The seed data includes one in
  `Europe/London` specifically so this is exercised by the demo data, not just
  by tests.
- `starts_at` on a request may carry any offset — it identifies an instant.

### Bad

- A naive timestamp (`2026-09-07T09:00:00`) is **rejected**, which surprises some
  clients. The alternative is guessing a timezone and silently booking the wrong
  hour when the guess is wrong. Documented prominently in `docs/api.md`.
- Working-hours intervals **cannot cross midnight**. This keeps "which local day
  does this slot belong to?" a single-day question everywhere. An overnight
  clinic would need this revisited.
- Around a fall-back, an ambiguous local time resolves to the first of the two
  candidate offsets (Go's `time.Date` behaviour). Deterministic and documented,
  but arbitrary.

## Verification

- `TestPolicyAcrossDSTTransition` — slot count, local wall clock and UTC instant
  either side of the Europe/London transition.
- `TestScheduleWindowsOn/a DST transition changes the window's real duration` —
  eight wall-clock hours across a spring-forward are seven real hours.
- `TestPolicyFreeSlotsOn/every offered slot passes validation` — feeds the
  availability output back through the booking validator, which is the guarantee
  that makes availability trustworthy.
- `TestDateOf` — an instant near midnight belongs to different calendar days in
  different zones.
- `TestDoctorLocation/the zone database is embedded in the binary` — the
  production image will be distroless with no system tzdata.
