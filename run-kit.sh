#!/bin/bash
# Run the kit conformance + perf suite against a fresh forge-server.
# Usage: run-kit.sh <kitdir> <outfile>
#   kitdir: kit copy to use (practice or new-seed)
#   outfile: where the JSON result goes
set -u
KIT="$1"
OUT="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"
BUILD="$HOME/workspace/makefun/gitforge-build"
TRANSPORT="${TRANSPORT:-raw}"
cd "$BUILD" || exit 1
./forge-server --transport "$TRANSPORT" --port 0 > /tmp/kit-server.log 2>&1 &
SRV=$!
PORT=""
for i in $(seq 1 40); do
  PORT=$(grep -o "listening on port [0-9]*" /tmp/kit-server.log 2>/dev/null | grep -o "[0-9]*$" | head -1)
  [ -n "$PORT" ] && break
  sleep 0.5
done
if [ -z "$PORT" ]; then echo "server did not start"; cat /tmp/kit-server.log; kill $SRV 2>/dev/null; exit 1; fi
for i in $(seq 1 40); do curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.5; done
cd "$KIT" || exit 1
node --experimental-strip-types sdk/run.ts --url "http://127.0.0.1:$PORT" --token test-token --perf --json > "$OUT" 2>/tmp/kit-run.err
CODE=$?
kill $SRV 2>/dev/null
sleep 1
echo "kit exit: $CODE, output: $OUT"
node -e "const r=require('$OUT'); console.log('passed:', r.counts.passed+'/'+r.counts.registered, 'perf:', JSON.stringify((r.perf.workloads||[]).map(w=>w.name+':p50='+w.p50_ms+',pts='+w.points)))" 2>/dev/null || { echo "--- kit stderr:"; tail -5 /tmp/kit-run.err; }
