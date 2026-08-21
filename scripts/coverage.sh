#!/usr/bin/env bash
# Measure test coverage and enforce a threshold on the business-critical packages.
#
# Coverage is enforced per package rather than as a module-wide average. A single
# number over the whole module is easy to inflate with generated code and trivial
# wiring, and would let the booking rules rot while the percentage stayed green.
#
# Usage: coverage.sh <profile-path> <threshold-percent> <package>...

set -euo pipefail

profile="${1:?usage: coverage.sh <profile> <threshold> <package>...}"
threshold="${2:?usage: coverage.sh <profile> <threshold> <package>...}"
shift 2
critical=("$@")

if [ ${#critical[@]} -eq 0 ]; then
  echo "ERROR: no critical packages given" >&2
  exit 2
fi

# Both suites contribute to one profile. The integration tests exercise the
# postgres adapter and a good deal of the service, so measuring only the unit
# suite would understate real coverage.
echo "==> Running tests with coverage"
go test \
  -tags=integration \
  -race \
  -count=1 \
  -covermode=atomic \
  -coverprofile="$profile" \
  -coverpkg=./internal/...,./api/... \
  ./... > /dev/null

echo ""
echo "==> Coverage by package"
go tool cover -func="$profile" \
  | awk '
      $1 ~ /\.go:/ {
        split($1, parts, ":");
        path = parts[1];
        sub(/\/[^\/]*$/, "", path);
        pct = $NF; gsub("%", "", pct);
        sum[path] += pct; count[path] += 1;
      }
      END { for (p in count) printf "%7.1f%%  %s\n", sum[p] / count[p], p }
    ' \
  | sort -k2

total=$(go tool cover -func="$profile" | awk '/^total:/ {gsub("%","",$NF); print $NF}')
echo ""
echo "==> Total: ${total}%"

echo ""
echo "==> Enforcing >= ${threshold}% on business-critical packages"
failed=0
for pkg in "${critical[@]}"; do
  pct=$(go tool cover -func="$profile" \
    | awk -v pkg="$pkg/" '
        index($1, pkg) == 1 && $1 ~ /\.go:/ {
          # Only files directly in the package, not in a subdirectory.
          rest = substr($1, length(pkg) + 1);
          if (index(rest, "/") > 0) next;
          v = $NF; gsub("%", "", v); sum += v; n += 1;
        }
        END { if (n > 0) printf "%.1f", sum / n; else print "none" }
      ')

  if [ "$pct" = "none" ]; then
    printf '  \033[31m✗\033[0m %-58s no coverage data\n' "$pkg"
    failed=1
    continue
  fi

  if awk -v a="$pct" -v b="$threshold" 'BEGIN { exit !(a + 0 >= b + 0) }'; then
    printf '  \033[32m✓\033[0m %-58s %5s%%\n' "$pkg" "$pct"
  else
    printf '  \033[31m✗\033[0m %-58s %5s%% (below %s%%)\n' "$pkg" "$pct" "$threshold"
    failed=1
  fi
done

echo ""
if [ "$failed" -ne 0 ]; then
  echo "Coverage gate FAILED. Add tests for the packages marked above."
  exit 1
fi
echo "Coverage gate passed."
