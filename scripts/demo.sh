#!/usr/bin/env bash
# The whole lifecycle of the first executable version, on this machine, for a person to
# watch: import an original, ingest, search, a clip through two approval gates, publish
# to a mock destination, the public library, withdrawal, the audit chain, a crash.
#
#   scripts/demo.sh            run it, then stop everything and remove what it made
#   scripts/demo.sh --keep     leave the temporary directory for inspection
#   scripts/demo.sh --pause    wait for Enter between steps
#
# What is real here: the three binaries over the real API, SQLite, Redis, FFmpeg, the
# Python worker process, the Next.js library. What is NOT: the AI engine is the FAKE one
# (canned transcript, word-hash embeddings), the LLM is a labeled mock, the destination
# is the mock driver, and storage is the local driver. No cloud account, no paid call.
# Nothing here says anything about real AI quality. See docs/status.md.
#
# Needs: go, ffmpeg, ffprobe, python3, node, npm, sqlite3, and redis-server (on PATH, or
# through `mise exec redis@8.8.0`). Credentials stay in a plaintext file inside the
# temporary directory (a test-only keyring build), never in your OS keyring.
set -euo pipefail

KEEP=0
PAUSE=0
for arg in "$@"; do
  case "$arg" in
  --keep) KEEP=1 ;;
  --pause) PAUSE=1 ;;
  -h | --help)
    sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  *)
    echo "unknown argument: $arg" >&2
    exit 2
    ;;
  esac
done

REPO=$(cd "$(dirname "$0")/.." && pwd -P)

if [ -t 1 ]; then
  BOLD=$'\033[1m' DIM=$'\033[2m' GREEN=$'\033[32m' RED=$'\033[31m' RESET=$'\033[0m'
else
  BOLD='' DIM='' GREEN='' RED='' RESET=''
fi

step() {
  echo
  echo "${BOLD}== $* ==${RESET}"
  if [ "$PAUSE" -eq 1 ]; then
    read -r -p "press Enter to continue " _ </dev/tty || true
  fi
}
note() { echo "${DIM}$*${RESET}"; }
good() { echo "${GREEN}ok:${RESET} $*"; }
fail() {
  echo "${RED}FAILED:${RESET} $*" >&2
  show_logs
  exit 1
}

# ---- preconditions ----

need() { command -v "$1" >/dev/null 2>&1 || {
  echo "demo.sh needs $1 on PATH" >&2
  exit 1
}; }
for tool in go ffmpeg ffprobe python3 node npm sqlite3 curl; do need "$tool"; done

REDIS_SERVER=()
REDIS_CLI=()
if command -v redis-server >/dev/null 2>&1 && redis-server --version >/dev/null 2>&1; then
  REDIS_SERVER=(redis-server)
  REDIS_CLI=(redis-cli)
elif command -v mise >/dev/null 2>&1 && mise exec redis@8.8.0 -- redis-server --version >/dev/null 2>&1; then
  REDIS_SERVER=(mise exec redis@8.8.0 -- redis-server)
  REDIS_CLI=(mise exec redis@8.8.0 -- redis-cli)
else
  echo "demo.sh needs redis-server on PATH, or mise with redis@8.8.0" >&2
  exit 1
fi

# ---- the temporary world ----

# A short path: a Unix socket's must fit in about 100 bytes on macOS.
ROOT=$(mktemp -d /tmp/vdmdemo.XXXXXX)
chmod 700 "$ROOT"
PIDS=()

show_logs() {
  [ -d "$ROOT/logs" ] || return 0
  for f in "$ROOT"/logs/*.log; do
    [ -f "$f" ] || continue
    echo "--- $f" >&2
    tail -n 15 "$f" >&2
  done
}

cleanup() {
  code=$?
  set +e
  echo
  note "stopping everything this demo started"
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  # Give graceful shutdowns a moment, then make sure.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    alive=0
    for pid in "${PIDS[@]:-}"; do
      [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && alive=1
    done
    [ "$alive" -eq 0 ] && break
    sleep 1
  done
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null
  done
  # npm leaves the Next.js server behind when it is killed.
  if [ -n "${LIBRARY_PORT:-}" ] && command -v lsof >/dev/null 2>&1; then
    for pid in $(lsof -ti "tcp:$LIBRARY_PORT" -sTCP:LISTEN 2>/dev/null); do
      kill "$pid" 2>/dev/null
    done
  fi
  wait 2>/dev/null
  if [ "$KEEP" -eq 1 ]; then
    echo "kept $ROOT"
  else
    # The plugin store is read-only on purpose; nothing here uses it, but be safe.
    chmod -R u+w "$ROOT" 2>/dev/null
    rm -rf "$ROOT"
    note "removed $ROOT"
  fi
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

mkdir -p "$ROOT"/{bin,config,data,work,run,secrets,storage,ingest,logs,clients,mock,public,fixtures,plugins}
chmod 700 "$ROOT"/run "$ROOT"/data

free_port() {
  python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}
REDIS_PORT=$(free_port)
HTTPS_PORT=$(free_port)
MOCK_PORT=$(free_port)
LIBRARY_PORT=$(free_port)

spawn() { # spawn NAME COMMAND...
  local name=$1
  shift
  "$@" >>"$ROOT/logs/$name.log" 2>&1 &
  PIDS+=($!)
  LAST_PID=$!
}

json() { # json 'python expression on d' reads a JSON document from stdin
  python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"
}

# Each person has their own configuration directory and keyring file.
as() { # as USER vidyom-args...
  local who=$1
  shift
  VIDYOM_CONFIG_DIR="$ROOT/clients/$who" VIDYOM_TEST_KEYRING_FILE="$ROOT/clients/$who.keyring" \
    "$ROOT/bin/vidyom" "$@" 2> >(grep -v '^warning: test build' >&2)
}
say() { # say USER vidyom-args...: show the command, then run it
  local who=$1
  shift
  echo "${DIM}[$who]${RESET} ${BOLD}vidyom $*${RESET}"
  as "$who" "$@"
}

wait_job() { # wait_job USER JOB STATUS
  local who=$1 job=$2 want=$3 status
  for _ in $(seq 1 600); do
    status=$(as "$who" --json job status "$job" | json 'd["status"]')
    [ "$status" = "$want" ] && return 0
    case "$status" in
    failed | cancelled | needs_attention)
      as "$who" job status "$job" >&2
      fail "$job ended $status"
      ;;
    esac
    sleep 1
  done
  fail "$job never reached $want (it is $status)"
}

# ---- step 1 ----

step "Build the binaries"
cd "$REPO"
export CGO_CFLAGS='-g -O2 -Wno-deprecated-declarations'
# The linker warns about a duplicate library on macOS, harmlessly. A real failure still stops here.
go build -trimpath -o "$ROOT/bin/vidyomd" ./cmd/vidyomd 2> >(grep -v 'duplicate libraries\|^#' >&2 || true)
CGO_ENABLED=0 go build -trimpath -o "$ROOT/bin/vidyom-worker" ./cmd/vidyom-worker
CGO_ENABLED=0 go build -trimpath -tags vidyom_test_keyring -o "$ROOT/bin/vidyom" ./cmd/vidyom
CGO_ENABLED=0 go build -trimpath -o "$ROOT/bin/vidyom-mock-llm" ./connectors/ai/openai/mockllm/cmd/vidyom-mock-llm
if [ ! -f library/.next/BUILD_ID ]; then
  note "building the public library, once"
  [ -d library/node_modules ] || (cd library && npm ci --no-audit --no-fund --silent)
  (cd library && NEXT_TELEMETRY_DISABLED=1 npm run build >/dev/null)
fi
good "vidyomd, vidyom-worker, vidyom, a labeled mock LLM"

step "1. Start Redis, initialize, refuse to start without a master key, start everything"
(cd config/local && find . -type f) | while read -r file; do
  mkdir -p "$ROOT/config/$(dirname "$file")"
  sed -e "s#@ROOT@#$ROOT#g" -e "s#@REDIS_PORT@#$REDIS_PORT#g" -e "s#@HTTPS_PORT@#$HTTPS_PORT#g" \
    -e "s#@MOCK_LLM_PORT@#$MOCK_PORT#g" "config/local/$file" >"$ROOT/config/$file"
done
note "configuration rendered from config/local into $ROOT/config"

spawn redis "${REDIS_SERVER[@]}" --port "$REDIS_PORT" --bind 127.0.0.1 --appendonly yes --dir "$ROOT/work" --save ""
for _ in $(seq 1 30); do
  [ "$("${REDIS_CLI[@]}" -p "$REDIS_PORT" ping 2>/dev/null)" = "PONG" ] && break
  sleep 1
done

"$ROOT/bin/vidyomd" init --generate-master-key "$ROOT/secrets/master.key" 2>/dev/null
"$ROOT/bin/vidyomd" init --config-dir "$ROOT/config" --tenant acme --admin admin@example.com --json >"$ROOT/init.json"
CODE=$(json 'd["pairing_code"]' <"$ROOT/init.json")
PIN=$(json 'd["tls_fingerprint"]' <"$ROOT/init.json")
good "vidyomd init: system.db, tenant acme, first admin, one pairing code"

mkdir -p "$ROOT/config-nokey"
python3 - "$ROOT/config/daemon.json" "$ROOT/config-nokey/daemon.json" <<'PY'
import json, sys
config = json.load(open(sys.argv[1]))
del config["vault"]
json.dump(config, open(sys.argv[2], "w"))
PY
if output=$("$ROOT/bin/vidyomd" start --config-dir "$ROOT/config-nokey" 2>&1); then
  fail "the daemon started with no master key configured"
fi
good "with no master key configured it refuses: $output"

spawn mock-llm "$ROOT/bin/vidyom-mock-llm" --listen "127.0.0.1:$MOCK_PORT"
spawn vidyomd "$ROOT/bin/vidyomd" start --config-dir "$ROOT/config"
DAEMON_PID=$LAST_PID
for _ in $(seq 1 30); do
  [ -S "$ROOT/run/vidyomd.sock" ] && break
  sleep 1
done
[ -S "$ROOT/run/vidyomd.sock" ] || fail "the daemon did not start"

(cd aiworker && PYTHONDONTWRITEBYTECODE=1 exec python3 -m vidyom --engine fake --socket "$ROOT/run/ai.sock") >>"$ROOT/logs/ai-python.log" 2>&1 &
PIDS+=($!)
start_workers() {
  spawn worker-media "$ROOT/bin/vidyom-worker" --config "$ROOT/config/daemon.json" --pools media \
    --token-file "$ROOT/data/secrets/worker.token" --cpu-cores 8 --memory-mb 16384 --disk-mb 102400
  MEDIA_PID=$LAST_PID
  spawn worker-ai "$ROOT/bin/vidyom-worker" --config "$ROOT/config/daemon.json" --pools ai \
    --token-file "$ROOT/data/secrets/worker.token" --cpu-cores 8 --memory-mb 16384 --disk-mb 102400
  AI_PID=$LAST_PID
}
start_workers
curl -sk "https://127.0.0.1:$HTTPS_PORT/api/v1/health" | json 'json.dumps(d)'
good "health answers over HTTPS"

# ---- step 2 ----

step "2. Pair clients, choose the tenant, add people"
mkdir -p "$ROOT/clients"/{admin,editor,publisher,viewer}
say admin server add main --url "https://127.0.0.1:$HTTPS_PORT" --fingerprint "$PIN"
say admin server pair main --code "$CODE"
say admin tenant use acme
pair_user() { # pair_user NAME ROLE
  local who=$1 role=$2 code
  say admin user add "$who@example.com" --role "$role"
  code=$(as admin --json device pairing-code create --user "$who@example.com" | json 'd["code"]')
  as "$who" server add main --url "https://127.0.0.1:$HTTPS_PORT" --fingerprint "$PIN" >/dev/null
  as "$who" server pair main --code "$code" >/dev/null
  as "$who" tenant use acme >/dev/null
  good "$who@example.com paired, role $role"
}
pair_user editor editor
pair_user publisher publisher
pair_user viewer viewer
TOKEN=$(python3 -c 'import json,sys; print(next(v for k,v in json.load(open(sys.argv[1])).items() if "device/" in k))' "$ROOT/clients/admin.keyring")
note "a request with a credential but no Vidyom-Tenant header:"
curl -sk -H "Authorization: Bearer $TOKEN" "https://127.0.0.1:$HTTPS_PORT/api/v1/assets"
good "refused: every tenant-scoped request names its tenant (D32)"

# ---- step 3 ----

step "3. Generate a fixture with FFmpeg, upload it, ingest it"
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=1280x720:rate=30:duration=120" \
  -f lavfi -i "sine=frequency=330:duration=120" -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
  -c:a aac -shortest "$ROOT/fixtures/fixture.mp4"
say editor original import "$ROOT/fixtures/fixture.mp4" --type yoga_class --title "Hatha Yoga Class" \
  --speaker "Maria Example" --workflow ingest-original
wait_job editor JOB-001 completed
say editor asset show ORI-001
good "ready_for_clipping: raw in archive, master in master, proxy in proxy"
note "The transcript is the fake engine's canned text, unrelated to this test tone."

# ---- step 4 ----

step "4. Search"
say viewer original search "conscious breathing" --asset ORI-001

# ---- step 5 ----

step "5. Ask for a clip; it stops at the selection gate"
say editor clip create --source ORI-001 --prompt "conscious breathing" --destination mock-main
wait_job editor JOB-002 waiting_approval
say editor approval show APR-001 | sed -n 1,12p
say viewer clip preview APR-001 --output "$ROOT/fixtures/candidate.mp4"
note "that candidate lasts $(ffprobe -v error -show_entries format=duration -of csv=p=0 "$ROOT/fixtures/candidate.mp4") s; open it with any player"

# ---- step 6 ----

step "6. A viewer is refused; the editor approves; the clip renders from master only"
if as viewer approval approve APR-001 2>/dev/null; then fail "a viewer approved"; fi
good "viewer refused"
say editor approval approve APR-001 --start 0 --end 110
for _ in $(seq 1 180); do
  status=$( (as editor --json asset show CLIP-001 | json 'd["proxy_available"] and d["master_available"] and d["backup_verified"]') 2>/dev/null || echo no)
  [ "$status" = "True" ] && break
  sleep 1
done
[ "$status" = "True" ] || fail "CLIP-001 did not appear"
say editor asset lineage CLIP-001
reads=$(python3 - "$ROOT/storage/access.log" <<'PY'
import json, sys
print(sum(1 for line in open(sys.argv[1]) if (e := json.loads(line))["role"] == "archive" and e["class"] == "read"))
PY
)
[ "$reads" = "0" ] || fail "something read from archive"
good "the storage access log shows no read of archive"

# ---- step 7 ----

step "7. The publication gate: the editor is refused, the publisher decides"
for _ in $(seq 1 180); do
  as publisher --json approval show APR-002 >/dev/null 2>&1 && break
  sleep 1
done
if as editor approval approve APR-002 2>/dev/null; then fail "an editor approved a publication"; fi
good "editor refused (D7)"
say publisher approval approve APR-002

# ---- step 8 ----

step "8. Publish to the mock destination"
for _ in $(seq 1 180); do
  status=$(as publisher --json publish status CLIP-001 | json 'd["items"][0]["status"] if d["items"] else ""')
  [ "$status" = "published" ] && break
  sleep 1
done
[ "$status" = "published" ] || fail "PUB-001 is $status"
say publisher publish status CLIP-001
note "uploads the mock destination received: $(json 'd["uploads"]' <"$ROOT/mock/acme/mock-main.json")"

# ---- step 9 ----

step "9. publication.completed exported public.db"
for _ in $(seq 1 120); do
  [ -f "$ROOT/public/acme/public.db" ] && break
  sleep 1
done
sqlite3 -header "$ROOT/public/acme/public.db" 'select public_id, title, kind, speaker from videos; select destination, url from destinations;'
if grep -aq "storage://\|CLIP-001\|ORI-001\|Hatha Yoga Class" "$ROOT/public/acme/public.db"; then
  fail "public.db holds private data"
fi
good "no storage key, internal ID or private original data in the file"

# ---- step 10 ----

step "10. The public library, from a server that has no idea the daemon exists"
(cd library && PORT=$LIBRARY_PORT HOSTNAME=127.0.0.1 NEXT_TELEMETRY_DISABLED=1 VIDYOM_PUBLIC_DB="$ROOT/public/acme/public.db" exec npm start) >>"$ROOT/logs/library.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 60); do
  curl -sf "http://127.0.0.1:$LIBRARY_PORT/api/health" >/dev/null 2>&1 && break
  sleep 1
done
note "curl http://127.0.0.1:$LIBRARY_PORT/api/search?q=conscious%20breathing"
curl -s "http://127.0.0.1:$LIBRARY_PORT/api/search?q=conscious%20breathing" |
  json 'json.dumps({"mode": d["mode"], "semantic": d["semantic"], "titles": [r["title"] for r in d["results"]]}, indent=1)'
note "With --keep --pause, the site itself is at http://127.0.0.1:$LIBRARY_PORT/ while this runs."

# ---- step 11 ----

step "11. Withdraw, twice"
say publisher unpublish CLIP-001 --all
wait_job publisher "$(as publisher --json job list --workflow unpublish-video --limit 1 | json 'd["items"][0]["id"]')" completed
for _ in $(seq 1 60); do
  [ "$(sqlite3 "$ROOT/public/acme/public.db" 'select count(*) from videos;')" = "0" ] && break
  sleep 1
done
[ "$(sqlite3 "$ROOT/public/acme/public.db" 'select count(*) from videos;')" = "0" ] || fail "the record is still in public.db"
good "gone from public.db"
note "curl the library again:"
curl -s "http://127.0.0.1:$LIBRARY_PORT/api/search?q=conscious%20breathing" | json 'json.dumps({"results": d["results"]})'
say publisher unpublish CLIP-001 --all
good "the second run is a no-op"

# ---- step 12 ----

step "12. The audit chain"
say admin security audit --verify
note "now tampering with a row, the way an attacker with host access would"
sqlite3 "$ROOT/data/tenants/acme/tenant.db" "DROP TRIGGER audit_log_no_update; UPDATE audit_log SET actor='somebody-else@example.com' WHERE seq=3;"
if as admin security audit --verify; then fail "a tampered chain verified"; fi
good "tampering makes verification fail"

# ---- step 13 ----

step "13. Resilience: kill -9 the daemon during a transcode, then FLUSHALL with a job queued"
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=1920x1080:rate=30:duration=240" \
  -f lavfi -i "sine=frequency=330:duration=240" -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
  -c:a aac -shortest "$ROOT/fixtures/heavy.mp4"
JOB=$(as editor --json original import "$ROOT/fixtures/heavy.mp4" --title "Long class" --type yoga_class | json 'd["job"]["job_id"]')
note "started $JOB"
for _ in $(seq 1 600); do
  state=$(as editor --json job status "$JOB" | json '[n["status"] for n in d["nodes"] if n["node_id"]=="compress"][0]')
  [ "$state" = "running" ] && break
  sleep 0.2
done
[ "$state" = "running" ] || fail "the transcode never started"
kill -9 "$DAEMON_PID"
good "kill -9 with the transcode running. The worker keeps encoding."
sleep 2
spawn vidyomd "$ROOT/bin/vidyomd" start --config-dir "$ROOT/config"
DAEMON_PID=$LAST_PID
wait_job editor "$JOB" completed
attempts=$(as editor --json job status "$JOB" | json 'sorted({n["attempt"] for n in d["nodes"]})')
good "$JOB completed after the restart; node attempts: $attempts"

kill "$MEDIA_PID" "$AI_PID"
wait "$MEDIA_PID" "$AI_PID" 2>/dev/null || true
ffmpeg -hide_banner -loglevel error -y -f lavfi -i "testsrc2=size=640x360:rate=25:duration=30" \
  -f lavfi -i "sine=frequency=330:duration=30" -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
  -c:a aac -shortest "$ROOT/fixtures/small.mp4"
JOB=$(as editor --json original import "$ROOT/fixtures/small.mp4" --title "Queued class" --type yoga_class | json 'd["job"]["job_id"]')
for _ in $(seq 1 60); do
  queued=$(as editor --json job status "$JOB" | json 'any(n["status"]=="queued" for n in d["nodes"])')
  [ "$queued" = "True" ] && break
  sleep 0.5
done
"${REDIS_CLI[@]}" -p "$REDIS_PORT" FLUSHALL
good "workers stopped, $JOB queued, Redis emptied"
start_workers
wait_job editor "$JOB" completed
good "reconciliation put the task back; $JOB completed"

echo
echo "${BOLD}${GREEN}The lifecycle ran end to end.${RESET}"
note "Reminder: the AI was the fake engine and the destination a mock. docs/status.md says what that does and does not prove."
