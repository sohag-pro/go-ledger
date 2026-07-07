#!/usr/bin/env bash
# Runs Go coverage over internal/, excludes generated packages, and enforces a
# global floor plus higher per-package floors on the money path. Statement
# weighted via `go tool cover -func`. Exits non-zero on any breach.
set -euo pipefail

PROFILE="${COVER_PROFILE:-cover.out}"
GLOBAL_FLOOR=80

# Per-package floors. Path suffix under github.com/sohag-pro/go-ledger/ to floor.
declare -A FLOORS=(
  ["internal/domain"]=90
  ["internal/ledger"]=85
  ["internal/postgres"]=80
)

# Generated packages excluded from the measured profile.
EXCLUDE='internal/genproto/|internal/postgres/sqlc/'

if [ ! -f "$PROFILE" ]; then
  echo "coverage: generating $PROFILE (set COVER_PROFILE to reuse a CI profile)"
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

# Per-package percentages from the filtered func report.
func_out="$(go tool cover -func="$FILTERED")"

for pkg in "${!FLOORS[@]}"; do
  # Average the statement percentages for files under this package. go tool
  # cover -func prints one line per function; the "total" line is global only,
  # so compute the package number from its function lines.
  pct="$(echo "$func_out" | grep "/$pkg/" | awk '{gsub("%","",$3); s+=$3; n++} END{if(n>0) printf "%.1f", s/n; else print "NA"}')"
  floor="${FLOORS[$pkg]}"
  printf '  %-40s %7s%% %7s%%\n' "$pkg" "$pct" "$floor"
  if [ "$pct" != "NA" ] && awk "BEGIN{exit !($pct < $floor)}"; then
    echo "  FAIL: $pkg at ${pct}% is below its ${floor}% floor"
    fail=1
  fi
done

total="$(echo "$func_out" | grep '^total:' | awk '{gsub("%","",$3); print $3}')"
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
