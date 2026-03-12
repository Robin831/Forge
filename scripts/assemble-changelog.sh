#!/usr/bin/env bash
# assemble-changelog.sh — Collects changelog.d/ fragments into CHANGELOG.md
#
# Usage: ./scripts/assemble-changelog.sh <version>
#   e.g. ./scripts/assemble-changelog.sh 0.2.0
#
# Fragments are categorised by their "category:" header line (Added, Changed,
# Fixed, Removed, etc.). After assembly the fragments are removed so they
# don't appear in subsequent releases.

set -euo pipefail

VERSION="${1:?Usage: assemble-changelog.sh <version>}"
CHANGELOG="CHANGELOG.md"
FRAGMENT_DIR="changelog.d"
DATE=$(date -u +%Y-%m-%d)

if [ ! -d "$FRAGMENT_DIR" ] || ! compgen -G "$FRAGMENT_DIR"/*.md > /dev/null; then
  echo "No changelog fragments found in $FRAGMENT_DIR/" >&2
  exit 0
fi

# Collect fragments grouped by category
declare -A CATEGORIES
for f in "$FRAGMENT_DIR"/*.md; do
  cat_line=$(head -1 "$f")
  category=$(echo "$cat_line" | sed 's/^category:[[:space:]]*//')
  # Everything after the first line is the content
  body=$(tail -n +2 "$f" | sed '/^$/d')
  if [ -n "${CATEGORIES[$category]+x}" ]; then
    CATEGORIES[$category]="${CATEGORIES[$category]}"$'\n'"$body"
  else
    CATEGORIES[$category]="$body"
  fi
done

# Build the new release section
SECTION="## [$VERSION] - $DATE"$'\n'
for category in Added Changed Deprecated Removed Fixed Security; do
  if [ -n "${CATEGORIES[$category]+x}" ]; then
    SECTION+=$'\n'"### $category"$'\n'$'\n'
    SECTION+="${CATEGORIES[$category]}"$'\n'
    unset "CATEGORIES[$category]"
  fi
done
# Any remaining non-standard categories (sorted for deterministic output)
for category in $(printf '%s\n' "${!CATEGORIES[@]}" | sort); do
  SECTION+=$'\n'"### $category"$'\n'$'\n'
  SECTION+="${CATEGORIES[$category]}"$'\n'
done

# Prepend to CHANGELOG.md (or create it)
if [ -f "$CHANGELOG" ]; then
  # Insert new section after the intro block, before the first release heading (## [)
  TMPFILE=$(mktemp)
  awk -v section="$SECTION" '
    BEGIN { inserted = 0 }
    NR == 1 { print; next }
    !inserted && $0 ~ /^## \[/ {
      print "";
      print section;
      inserted = 1;
    }
    { print }
    END {
      if (!inserted) {
        print "";
        print section;
      }
    }
  ' "$CHANGELOG" > "$TMPFILE"
  mv -f "$TMPFILE" "$CHANGELOG"
else
  {
    echo "# Changelog"
    echo ""
    echo "$SECTION"
  } > "$CHANGELOG"
fi

# Remove consumed fragments
rm -f "$FRAGMENT_DIR"/*.md

echo "Assembled $VERSION changelog from fragments into $CHANGELOG"
