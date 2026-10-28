#!/usr/bin/env bash
# Runs the k6 scenario against a running API, then the invariant checks.
#   BASE_URL   API base URL (default http://localhost:8080)
#   PSQL_URL   database URL for the checks (default: local compose database)
set -euo pipefail
cd "$(dirname "$0")"
BASE_URL=${BASE_URL:-http://localhost:8080}
PSQL_URL=${PSQL_URL:-postgres://dispatch:dispatch@localhost:5432/dispatch?sslmode=disable}
OUT=${OUT:-../../bin/loadtest}
mkdir -p "$OUT"

echo "== machine"
{
  echo "date: $(date -u +%Y-%m-%dT%H:%MZ)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2 | xargs || sysctl -n machdep.cpu.brand_string)"
  echo "cpus: $(nproc 2>/dev/null || sysctl -n hw.ncpu)"
  echo "ram_gb: $(awk '/MemTotal/ {printf "%.1f", $2/1048576}' /proc/meminfo 2>/dev/null || echo unknown)"
  echo "k6: $(k6 version | head -1)"
} | tee "$OUT/machine.txt"

echo "== k6"
k6 run --summary-export "$OUT/summary.json" -e BASE_URL="$BASE_URL" dispatch.js | tee "$OUT/k6.txt"

echo "== waiting 20 s for in-flight work to settle"
sleep 20

echo "== server counters"
curl -fsS "$BASE_URL/metrics" | grep -E '^dispatch_[a-z_]+(_total|_backlog|_vehicles|_depth) ' > "$OUT/server_metrics.txt" || true

echo "== invariants"
psql "$PSQL_URL" -f check_invariants.sql | tee "$OUT/invariants.txt"
status=0
if grep -qE '\| f *$' "$OUT/invariants.txt"; then
  echo "INVARIANT VIOLATION" >&2
  status=1
fi

echo "== report"
python3 report.py "$OUT" > ../../docs/loadtest.md
echo "wrote docs/loadtest.md"
exit $status
