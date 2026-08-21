# Onboarding rehearsal

A record of following **only the committed documentation** from a clean
checkout, to verify that the setup instructions actually work rather than
merely look plausible.

Re-run this after any change to the setup path, `compose.yaml`, the `Makefile`,
or the README quick start.

---

## Run of 2026-08-21

**Method.** A clean copy of the repository (exactly the 118 tracked
files, with no `.git`, no `.env` and no build output) placed in a
temporary directory. Every command below was taken verbatim from the committed
docs. Nothing from the development environment was reused except the Docker
daemon and the Go toolchain.

| # | Step | Source | Result |
|---|---|---|---|
| 1 | `make doctor` | [CONTRIBUTING](../CONTRIBUTING.md#setup-from-a-fresh-clone) | Passed. Correctly reported the one expected gap: *".env does not exist yet"*, with the fix |
| 2 | `cp .env.example .env` | [README quick start](../README.md#quick-start) | Passed |
| 3 | `docker compose up --build -d` | README quick start | Passed. **72 s** from a cold build to a serving API. Ordered chain observed: postgres healthy → migrate exited 0 → seed exited 0 → api started |
| 4 | `curl http://localhost:8080/readyz` | README quick start | `{"status":"ready","checks":{"accepting_traffic":"ok","database":"ok"},…}` |
| 5 | The README's availability example | README quick start | 14 slots, `Africa/Nairobi`, first at `09:00:00+03:00` |
| 6 | The README's booking example | README quick start | `HTTP 201`, a 30-minute appointment `06:00:00Z → 06:30:00Z` |
| 7 | `make smoke` | CONTRIBUTING | **17/17 checks passed** |
| 8 | `make unit-test` | CONTRIBUTING | All packages passed with `-race` |
| 9 | `make integration-test` | [testing](testing.md#integration-test-prerequisites) | Passed. Created `ratiba_test` itself, applied migrations, ran the concurrency suite |

**Total time to a working, verified system: a couple of minutes**, dominated by
the Docker image build.

> The README deliberately does **not** advertise a setup duration. 72 seconds was
> measured on one machine with a warm Docker layer cache and a fast connection; a
> cold module download on slower hardware will differ substantially. A promised
> number a reviewer fails to reproduce is worse than no number at all.

### Corrections made as a result

One, found by cross-checking the documentation against the Makefile rather than
by the rehearsal itself: `CONTRIBUTING.md` told a new contributor to run
`make dev`, and that target did not exist. It does now.

Everything else worked as written. No other documented command needed changing.

### What this does *not* prove

- **A truly cold start.** Go module and Docker base-image layers were already
  cached locally. A first-ever run adds the download time for both.
- **Another operating system.** Verified on macOS (darwin/arm64) only. The
  scripts are POSIX shell and the stack is containerised, but Linux and Windows
  (WSL2) are untested.
- **The deployment path.** Railway and GitHub were not exercised; see
  [deployment](deployment.md#verified-vs-not).

## How to repeat it

```bash
# From a fresh clone in a clean directory:
make doctor
cp .env.example .env
docker compose up --build -d
curl http://localhost:8080/readyz
make smoke
make unit-test
make integration-test
```

If any of these fail on a clean checkout, that is a documentation bug and should
be fixed in the same pull request as whatever broke it.
