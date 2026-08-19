# Ratiba — Clinic Appointment API

> *Ratiba* is Swahili for **schedule**.

A REST API for booking 30-minute clinic appointments, written in Go against
PostgreSQL. Built for the Savannah Informatics backend take-home assessment.

**Status:** in development. System design is being documented first, per
Section 1 of the brief.

## The problem

> *"We run a small clinic with 5 doctors. Patients need to book appointments
> online. Each doctor has set working hours and works in 30-minute slots. A
> patient should see which slots are free for a given doctor on a given day,
> pick one, and book it. Once booked, that slot must not be available to
> others. Patients should also be able to cancel. We're starting small but
> want to grow."*

## Planned endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/appointments` | Book a slot |
| `GET` | `/doctors/{id}/availability?date=` | Free slots on a date |
| `PATCH` | `/appointments/{id}/cancel` | Cancel with a reason |
| `PATCH` | `/appointments/{id}/reschedule` | Move to a new slot |
| `GET` | `/patients/{id}/appointments` | Upcoming appointments (bonus) |

## The hard part

Booking looks like CRUD until you notice the one real constraint: **two
patients must never hold the same slot**. That cannot be enforced by reading —
any availability check is stale the moment it returns. The design work is
deciding where that decision is made atomically.

## License

MIT. See [LICENSE](LICENSE).
