#!/usr/bin/env bash
# Registers N vehicles at random points in San Francisco: ./scripts/seed.sh 200
set -euo pipefail
N=${1:-200}
BASE_URL=${BASE_URL:-http://localhost:8080}
ok=0
for i in $(seq 1 "$N"); do
  lat=$(awk -v s="$RANDOM" 'BEGIN{srand(s); printf "%.5f", 37.70 + rand()*0.11}')
  lng=$(awk -v s="$RANDOM" 'BEGIN{srand(s); printf "%.5f", -122.52 + rand()*0.16}')
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/v1/vehicles" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"seed-v$i\",\"lat\":$lat,\"lng\":$lng,\"battery_pct\":90}")
  case $code in 201|409) ok=$((ok+1));; *) echo "vehicle seed-v$i: HTTP $code" >&2;; esac
done
echo "registered $ok of $N vehicles"
