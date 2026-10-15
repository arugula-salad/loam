#!/usr/bin/env bash
# Runs the official E2B Python and JS SDKs (pinned: requirements.txt, package.json) against
# loam's E2B adapter (docs/E2B.md, "Conformance"), 13 steps each: create, exec with streaming,
# exit codes, background processes, stdin and PTY, files, upload/download and signed URLs, a
# sandbox port, set-timeout, metrics, list, pause/resume and kill; then a sweep for sandboxes
# the probes left behind. The probes were first written against hosted E2B, and its traces of
# the same run are what they were checked against.
#
#   E2B_DOMAIN=e2b.loam.example.com E2B_API_KEY=loam_... e2e/e2b/run.sh
#                            a deployment (the key: any key of the project to run in)
#   E2B_DOMAIN=e2b.localhost:7901 E2B_API_URL=http://api.e2b.localhost:7901 \
#     E2B_SANDBOX_URL=http://api.e2b.localhost:7901 E2B_PROBE_PORT_SCHEME=http \
#     E2B_API_KEY=loam_... e2e/e2b/run.sh
#                            a local loam-api started with --e2b-domain e2b.localhost:7901,
#                            in plain HTTP (*.localhost must resolve to the loopback address)
#
# Other knobs: ONLY=py|js, E2B_PROBE_PAUSE=0 (skip pause/resume).
# Needs python3 and node. The venv lives in ${XDG_CACHE_HOME:-~/.cache}/loam/e2b-probe,
# node_modules next to this script. Exit status is the number of failures (sweep included).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/loam/e2b-probe"

: "${E2B_API_KEY:?set E2B_API_KEY to a loam project key}"
: "${E2B_DOMAIN:?set E2B_DOMAIN to the domain of the adapter, e.g. e2b.loam.example.com}"
export E2B_API_KEY E2B_DOMAIN

[ -x "$CACHE/venv/bin/python" ] || python3 -m venv "$CACHE/venv"
"$CACHE/venv/bin/python" -c 'import e2b' 2>/dev/null || "$CACHE/venv/bin/pip" install -q -r "$HERE/requirements.txt"
[ -d "$HERE/node_modules/e2b" ] || (cd "$HERE" && npm ci --no-audit --no-fund --silent)
PY="$CACHE/venv/bin/python"

out=$(mktemp)
trap 'rm -f "$out"' EXIT
for lang in py js; do
  [ -n "${ONLY:-}" ] && [ "$ONLY" != "$lang" ] && continue
  if [ "$lang" = py ]; then cmd=("$PY" "$HERE/probe.py"); else cmd=(node "$HERE/probe.mjs"); fi
  E2B_PROBE_RUN="$lang-$$" timeout 600 "${cmd[@]}" 2>&1 | tee -a "$out" || true
done
"$PY" "$HERE/sweep.py" 2>&1 | tee -a "$out" || true

fails=$(grep -cE ': FAILED|^\[sweep\] leaked|Traceback' "$out" || true)
oks=$(grep -cE '^\[(py|js)\] [a-z_]+: (OK|SKIP)' "$out" || true)
echo "== $oks step(s) OK/skipped, $fails failure(s)"
exit "$fails"
