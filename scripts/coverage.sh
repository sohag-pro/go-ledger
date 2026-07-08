#!/usr/bin/env bash
# Runs Go coverage over internal/, excludes generated packages, and enforces a
# global floor plus higher per-package floors on the money path. Both the
# global total and each per-package number are statement weighted: for each
# package we slice its lines out of the merged coverage profile and read the
# "total:" line that `go tool cover -func` reports for that slice, which is
# the same statement-weighted number `go test ./internal/<pkg>/... -cover`
# would report. Exits non-zero on any breach.
set -euo pipefail

GLOBAL_FLOOR=80

# Per-package floors as "path:floor" entries. Path suffix under
# github.com/sohag-pro/go-ledger/ to floor. Kept as a plain list (not an
# associative array) so this runs on bash 3.2, which is what ships on macOS.
FLOORS="internal/domain:90 internal/ledger:85 internal/postgres:80"

# Generated packages excluded from the measured profile.
EXCLUDE='internal/genproto/|internal/postgres/sqlc/'

# Only reuse an existing profile when the caller explicitly set
# COVER_PROFILE (the CI reuse path). Otherwise always regenerate, so a plain
# `make cover` measures current code instead of a stale cover.out left over
# from a previous run.
if [ -n "${COVER_PROFILE:-}" ]; then
  PROFILE="$COVER_PROFILE"
  if [ ! -f "$PROFILE" ]; then
    echo "coverage: generating $PROFILE"
    go test ./internal/... -covermode=atomic -coverprofile="$PROFILE"
  else
    echo "coverage: reusing existing $PROFILE (COVER_PROFILE set explicitly)"
  fi
else
  PROFILE="cover.out"
  echo "coverage: generating $PROFILE (fresh run; set COVER_PROFILE to reuse a profile)"
  go test ./internal/... -covermode=atomic -coverprofile="$PROFILE"
fi

# Strip generated packages from the profile (keep the mode header, line 1).
FILTERED="$(mktemp)"
head -n 1 "$PROFILE" > "$FILTERED"
grep -vE "$EXCLUDE" "$PROFILE" | tail -n +2 >> "$FILTERED" || true

fail=0
echo ""
echo "package coverage (generated excluded):"
printf '  %-40s %8s %8s\n' "PACKAGE" "COVER" "FLOOR"

for entry in $FLOORS; do
  pkg="$(echo "$entry" | cut -d: -f1)"
  floor="$(echo "$entry" | cut -d: -f2)"

  # Slice just this package's lines out of the filtered profile, then let
  # `go tool cover -func` compute a statement-weighted total for the slice.
  # This is the same computation `go test ./internal/<pkg>/... -cover` does,
  # so the number reported here matches that command (within rounding).
  PKG_PROFILE="$(mktemp)"
  head -n 1 "$FILTERED" > "$PKG_PROFILE"
  grep "/$pkg/" "$FILTERED" >> "$PKG_PROFILE" || true

  if [ "$(wc -l < "$PKG_PROFILE")" -gt 1 ]; then
    pct="$(go tool cover -func="$PKG_PROFILE" | awk '/^total:/{gsub("%","",$3); print $3}')"
  else
    pct="NA"
  fi
  rm -f "$PKG_PROFILE"

  printf '  %-40s %7s%% %7s%%\n' "$pkg" "$pct" "$floor"
  if [ "$pct" != "NA" ] && awk "BEGIN{exit !($pct < $floor)}"; then
    echo "  FAIL: $pkg at ${pct}% is below its ${floor}% floor"
    fail=1
  fi
done

total="$(go tool cover -func="$FILTERED" | awk '/^total:/{gsub("%","",$3); print $3}')"
printf '  %-40s %7s%% %7s%%\n' "TOTAL (internal/)" "$total" "$GLOBAL_FLOOR"
if awk "BEGIN{exit !($total < $GLOBAL_FLOOR)}"; then
  echo "  FAIL: total ${total}% is below the ${GLOBAL_FLOOR}% global floor"
  fail=1
fi

rm -f "$FILTERED"
echo ""
if [ "$fail" -ne 0 ]; then
  echo "coverage gate: FAIL"
  exit 1
fi
echo "coverage gate: PASS (total ${total}%)"
