#!/bin/bash
# Determinism check: run the sealed env (program/env/server.mjs) + the entry
# (main.ts) twice on the same instance and compare the state exports.
# Usage: run-det.sh <run-name> ; writes exports to /tmp/det-<run-name>/export/
set -u
KIT="$HOME/workspace/makefun/gitforge-kit/github-core-clone-01-kit"
BUILD="$HOME/workspace/makefun/gitforge-build"
RUN="$1"
STATE="/tmp/det-$RUN"
rm -rf "$STATE"
mkdir -p "$STATE"
cd "$KIT" || exit 1
MAKEFUN_STATE_DIR="$STATE" MAKEFUN_SHOP_TOKEN="det-test-token" node program/env/server.mjs > "$STATE/env.log" 2>&1 &
ENV_PID=$!
# wait for the env ready file
for i in $(seq 1 60); do
  [ -f "$STATE/ready" ] && break
  sleep 1
done
if [ ! -f "$STATE/ready" ]; then echo "ENV DID NOT BECOME READY"; cat "$STATE/env.log"; kill $ENV_PID 2>/dev/null; exit 1; fi
cd "$BUILD" || exit 1
MAKEFUN_ENV_URL="http://127.0.0.1:8080" MAKEFUN_SHOP_TOKEN="det-test-token" timeout 900 node main.ts > "$STATE/entry.log" 2>&1
ENTRY_CODE=$?
echo "entry exit code: $ENTRY_CODE"
sleep 2
kill -TERM $ENV_PID 2>/dev/null
sleep 2
kill -KILL $ENV_PID 2>/dev/null
ls -la "$STATE/export/" 2>/dev/null
echo "--- state.json sha256 ---"
sha256sum "$STATE/export/state.json" 2>/dev/null
echo "--- perf.json points ---"
node -e "const p=require('$STATE/export/perf.json'); console.log(JSON.stringify({points_total:p.points_total,workloads:p.workloads},null,1))" 2>/dev/null || cat "$STATE/export/perf.json" 2>/dev/null | head -20
