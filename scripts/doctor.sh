#!/usr/bin/env bash
# Diagnose local prerequisites and say exactly what to do about each problem.
#
# A new contributor should be able to run `make doctor` and get a definitive
# answer about whether their machine is ready, rather than discovering each
# missing tool one confusing error at a time.

set -uo pipefail

problems=0
warnings=0

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; problems=$((problems + 1)); }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; warnings=$((warnings + 1)); }
fix()  { printf '      → %s\n' "$1"; }

echo "Ratiba environment check"
echo ""

# --- Go ---------------------------------------------------------------------
required_go=$(awk '/^go /{print $2}' go.mod)
if ! command -v go >/dev/null 2>&1; then
  bad "Go is not installed (go.mod requires $required_go or newer)"
  fix "Install from https://go.dev/dl/ or: brew install go"
else
  installed_go=$(go version | awk '{print $3}' | sed 's/^go//')
  # Sort both versions and check the required one comes first.
  if [ "$(printf '%s\n%s\n' "$required_go" "$installed_go" | sort -V | head -1)" = "$required_go" ]; then
    ok "Go $installed_go (go.mod requires $required_go)"
  else
    bad "Go $installed_go is older than the $required_go required by go.mod"
    fix "Upgrade Go, or run the stack with Docker instead: make up"
  fi
fi

# --- Docker -----------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  bad "Docker is not installed"
  fix "Install Docker Desktop from https://docs.docker.com/get-docker/"
else
  if docker info >/dev/null 2>&1; then
    ok "Docker $(docker version --format '{{.Server.Version}}' 2>/dev/null) is running"
  else
    bad "Docker is installed but the daemon is not running"
    fix "Start Docker Desktop, then re-run: make doctor"
  fi

  if docker compose version >/dev/null 2>&1; then
    ok "Docker Compose $(docker compose version --short 2>/dev/null)"
  else
    bad "The 'docker compose' plugin is unavailable"
    fix "Update Docker Desktop, or install the compose plugin"
  fi
fi

# --- Optional development tools --------------------------------------------
# These are only needed to regenerate code or run the linters; the application
# builds, runs and tests without them.
if command -v sqlc >/dev/null 2>&1; then
  ok "sqlc $(sqlc version 2>/dev/null)"
else
  warn "sqlc is not installed (only needed for 'make generate')"
  fix "make bootstrap"
fi

if command -v golangci-lint >/dev/null 2>&1; then
  ok "golangci-lint $(golangci-lint version --short 2>/dev/null || echo installed)"
else
  warn "golangci-lint is not installed (only needed for 'make lint')"
  fix "make bootstrap"
fi

if command -v govulncheck >/dev/null 2>&1; then
  ok "govulncheck is installed"
else
  warn "govulncheck is not installed (only needed for 'make vulncheck')"
  fix "make bootstrap"
fi

if command -v psql >/dev/null 2>&1; then
  ok "psql is available"
else
  warn "psql is not installed (only needed for 'make psql'; the stack works without it)"
  fix "brew install libpq  # or your platform's postgresql-client"
fi

# --- Project state ----------------------------------------------------------
if [ -f .env ]; then
  ok ".env exists"
else
  warn ".env does not exist yet"
  fix "cp .env.example .env   (or run: make bootstrap)"
fi

if [ -f .env ] && git check-ignore -q .env 2>/dev/null; then
  ok ".env is git-ignored"
elif [ -f .env ]; then
  bad ".env is NOT git-ignored — it could be committed"
  fix "Check .gitignore contains a .env entry"
fi

echo ""
if [ "$problems" -gt 0 ]; then
  echo "$problems blocking problem(s), $warnings warning(s). Fix the ✗ items above."
  exit 1
fi
if [ "$warnings" -gt 0 ]; then
  echo "Ready to run. $warnings optional tool(s) missing — see the ! items above."
else
  echo "Everything is ready. Next: make dev"
fi
exit 0
