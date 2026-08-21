#!/usr/bin/env bash
# Check that every relative link in the documentation resolves to a real file.
#
# Documentation with broken links is worse than none: a reader follows a promise
# to a 404 and stops trusting the rest. This runs in CI, so a renamed file
# cannot quietly orphan the links pointing at it.
#
# External (http/https) links are not fetched — a network check would make CI
# flaky and dependent on third-party uptime.

set -uo pipefail

problems=0
checked=0

echo "==> Checking relative links in Markdown files"

while IFS= read -r file; do
  dir=$(dirname "$file")

  # Extract the target of every [text](target) link.
  while IFS= read -r target; do
    [ -n "$target" ] || continue

    # Skip external links, anchors and mailto.
    case "$target" in
      http://*|https://*|mailto:*|"#"*) continue ;;
    esac

    # Strip any anchor fragment; only the file needs to exist.
    path="${target%%#*}"
    [ -n "$path" ] || continue

    checked=$((checked + 1))

    if [ "${path:0:1}" = "/" ]; then
      resolved=".${path}"
    else
      resolved="$dir/$path"
    fi

    if [ ! -e "$resolved" ]; then
      printf '  \033[31m✗\033[0m %s → %s (no such file)\n' "$file" "$target"
      problems=$((problems + 1))
    fi
  done < <(grep -oE '\]\([^)]+\)' "$file" | sed 's/^](//; s/)$//')
done < <(find . -name '*.md' -not -path './.git/*' -not -path './node_modules/*' | sort)

echo ""
if [ "$problems" -gt 0 ]; then
  echo "$problems broken link(s) out of $checked checked."
  exit 1
fi
echo "$checked relative link(s) checked, all resolve."
