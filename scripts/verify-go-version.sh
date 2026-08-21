#!/usr/bin/env bash
# Check that go.mod, the Dockerfile and CI agree on the Go version.
#
# A drift here is nasty: CI passes on one toolchain, the image is built with
# another, and a version-specific bug only appears in the deployed service.
# go.mod is the single source of truth; everything else must match it.

set -euo pipefail

problems=0

go_mod_version=$(awk '/^go /{print $2}' go.mod)
echo "==> go.mod requires Go $go_mod_version"

dockerfile_version=$(awk -F= '/^ARG GO_VERSION=/{print $2}' Dockerfile)
if [ "$dockerfile_version" = "$go_mod_version" ]; then
  printf '  \033[32m✓\033[0m Dockerfile ARG GO_VERSION=%s\n' "$dockerfile_version"
else
  printf '  \033[31m✗\033[0m Dockerfile has GO_VERSION=%s, expected %s\n' "$dockerfile_version" "$go_mod_version"
  problems=$((problems + 1))
fi

# CI reads the version from go.mod via setup-go's go-version-file, which is the
# most robust arrangement. Accept either that or an exact literal match.
for workflow in .github/workflows/*.yml; do
  [ -f "$workflow" ] || continue
  if grep -q 'go-version-file:.*go\.mod' "$workflow"; then
    printf '  \033[32m✓\033[0m %s reads the version from go.mod\n' "$(basename "$workflow")"
  elif grep -q 'go-version:' "$workflow"; then
    ci_version=$(grep -m1 'go-version:' "$workflow" | sed "s/.*go-version:[[:space:]]*['\"]\?\([0-9.]*\).*/\1/")
    if [ "$ci_version" = "$go_mod_version" ]; then
      printf '  \033[32m✓\033[0m %s pins Go %s\n' "$(basename "$workflow")" "$ci_version"
    else
      printf '  \033[31m✗\033[0m %s pins Go %s, expected %s\n' "$(basename "$workflow")" "$ci_version" "$go_mod_version"
      problems=$((problems + 1))
    fi
  fi
done

echo ""
if [ "$problems" -gt 0 ]; then
  echo "Go version mismatch. Update the files above to match go.mod ($go_mod_version)."
  exit 1
fi
echo "Go version is consistent everywhere."
