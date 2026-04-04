#!/usr/bin/env bash
# =============================================================================
# coroTracer – full test suite
#
# Phases:
#   1. Go unit tests  (structure / engine / export / main)
#   2. Rust SDK unit tests  (SDK/rust  cargo test)
#   3. Build Go tracer binary
#   4. Build Rust integration tracee
#   5. Integration run  (Go engine + Rust tracee under coroTracer)
#   6. Verify JSONL output invariants
#   7. CSV export round-trip
#   8. SQLite export round-trip  (skipped if sqlite3 not in PATH)
#
# Exit code 0 = all required phases passed.
# =============================================================================

set -euo pipefail

# ─── Paths ────────────────────────────────────────────────────────────────────
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TESTS_DIR="$ROOT/tests"
OUTPUT_DIR="$TESTS_DIR/output"
TRACER_BIN="$ROOT/coroTracer"
TRACEE_BIN="$TESTS_DIR/rust_tracee/target/debug/rust_tracee"
TRACE_JSONL="$OUTPUT_DIR/trace.jsonl"
SHM_PATH="/tmp/corotracer_ci.shm"
SOCK_PATH="/tmp/corotracer_ci.sock"

mkdir -p "$OUTPUT_DIR"

# ─── Colour helpers ───────────────────────────────────────────────────────────
if [ -t 1 ]; then
    GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
else
    GREEN=''; RED=''; YELLOW=''; CYAN=''; NC=''
fi

PASS_COUNT=0
SKIP_COUNT=0

pass()  { echo -e "${GREEN}[PASS]${NC} $1"; (( PASS_COUNT++ )) || true; }
fail()  { echo -e "${RED}[FAIL]${NC} $1"; exit 1; }
info()  { echo -e "${CYAN}[INFO]${NC} $1"; }
skip()  { echo -e "${YELLOW}[SKIP]${NC} $1"; (( SKIP_COUNT++ )) || true; }
phase() { echo; echo -e "${YELLOW}══ Phase $1: $2 ══${NC}"; }

# ─── Phase 1: Go unit tests ───────────────────────────────────────────────────
phase 1 "Go unit tests"
cd "$ROOT"

GO_LOG="$OUTPUT_DIR/go_unit_tests.log"
if go test -count=1 -race ./... 2>&1 | tee "$GO_LOG"; then
    pass "Go unit tests (structure / engine / export / main)"
else
    fail "Go unit tests failed – see $GO_LOG"
fi

# ─── Phase 2: Rust SDK unit tests ─────────────────────────────────────────────
phase 2 "Rust SDK unit tests"
cd "$ROOT/SDK/rust"

RUST_SDK_LOG="$OUTPUT_DIR/rust_sdk_tests.log"
if cargo test 2>&1 | tee "$RUST_SDK_LOG"; then
    pass "Rust SDK unit tests (protocol_layout / TracedFuture semantics / Send)"
else
    fail "Rust SDK unit tests failed – see $RUST_SDK_LOG"
fi

# ─── Phase 3: Build Go tracer binary (always rebuild to stay in sync with source)
phase 3 "Go tracer binary"
cd "$ROOT"

if go build -o "$TRACER_BIN" .; then
    pass "Go tracer built → $TRACER_BIN"
else
    fail "go build failed"
fi

# ─── Phase 4: Rust integration tracee (build if missing) ─────────────────────
# cargo is incremental, but the first build on a clean machine is slow (~30s).
# Skip only when the binary already exists; cargo rebuild is instant when
# nothing changed.
phase 4 "Rust integration tracee"
cd "$TESTS_DIR/rust_tracee"

RUST_BUILD_LOG="$OUTPUT_DIR/rust_tracee_build.log"
if [ -f "$TRACEE_BIN" ]; then
    pass "Rust tracee already exists, skipping build → $TRACEE_BIN"
else
    info "Binary not found, building..."
    if cargo build 2>&1 | tee "$RUST_BUILD_LOG"; then
        pass "Rust tracee built → $TRACEE_BIN"
    else
        fail "Rust tracee build failed – see $RUST_BUILD_LOG"
    fi
fi

# ─── Phase 5: Rust tracee unit tests (cargo test) ─────────────────────────────
phase 5 "Rust tracee unit tests (cargo test)"
cd "$TESTS_DIR/rust_tracee"

RUST_TRACEE_TEST_LOG="$OUTPUT_DIR/rust_tracee_tests.log"
if cargo test 2>&1 | tee "$RUST_TRACEE_TEST_LOG"; then
    pass "Rust tracee unit tests"
else
    fail "Rust tracee unit tests failed – see $RUST_TRACEE_TEST_LOG"
fi

# ─── Phase 6: Integration run ─────────────────────────────────────────────────
phase 6 "Integration run (Go engine + Rust tracee)"
cd "$ROOT"

rm -f "$TRACE_JSONL" "$SHM_PATH"
INTEGRATION_LOG="$OUTPUT_DIR/integration_run.log"

info "Starting: coroTracer -n 256 -cmd $TRACEE_BIN"
"$TRACER_BIN" \
    -n 256 \
    -cmd "$TRACEE_BIN" \
    -shm "$SHM_PATH" \
    -sock "$SOCK_PATH" \
    -out "$TRACE_JSONL" 2>&1 | tee "$INTEGRATION_LOG"

TRACER_EXIT=${PIPESTATUS[0]}
if [ "$TRACER_EXIT" -ne 0 ]; then
    fail "coroTracer exited with code $TRACER_EXIT"
fi

# Confirm Rust side reported success
if ! grep -q "ALL_SCENARIOS_PASSED" "$INTEGRATION_LOG"; then
    fail "Rust tracee did not print ALL_SCENARIOS_PASSED – check $INTEGRATION_LOG"
fi
pass "Rust tracee completed all 12 scenarios"

# ─── Phase 7: Verify JSONL output invariants ─────────────────────────────────
phase 7 "Verify JSONL output"

if [ ! -f "$TRACE_JSONL" ]; then
    fail "JSONL output not found: $TRACE_JSONL"
fi

LINE_COUNT=$(grep -c . "$TRACE_JSONL" || true)
if [ "$LINE_COUNT" -lt 1 ]; then
    fail "JSONL file is empty"
fi
pass "JSONL has $LINE_COUNT event lines"

# All 6 required fields present on line 1
FIRST_LINE=$(head -1 "$TRACE_JSONL")
for field in probe_id tid addr seq is_active ts; do
    if ! echo "$FIRST_LINE" | grep -q "\"$field\""; then
        fail "JSONL line 1 missing field: $field"
    fi
done
pass "All required fields present (probe_id tid addr seq is_active ts)"

# Both suspend (is_active:false) and resume (is_active:true) events must exist
if ! grep -q '"is_active":false' "$TRACE_JSONL"; then
    fail "No suspend events (is_active:false) found in JSONL"
fi
if ! grep -q '"is_active":true' "$TRACE_JSONL"; then
    fail "No resume events (is_active:true) found in JSONL"
fi
pass "Both suspend and resume events present"

# addr must be 0x-prefixed hex (18 chars = "0x" + 16 hex digits)
ADDR_SAMPLE=$(grep -o '"addr":"0x[0-9a-f]\{16\}"' "$TRACE_JSONL" | head -1 || true)
if [ -z "$ADDR_SAMPLE" ]; then
    fail "No valid 0x-prefixed 64-bit addr values found in JSONL"
fi
pass "addr format correct: $ADDR_SAMPLE"

# seq values must be even (SeqLock invariant)
BAD_SEQ_COUNT=$(grep -o '"seq":[0-9]*' "$TRACE_JSONL" \
    | awk -F: '{if ($2 % 2 != 0) print}' | wc -l || true)
if [ "$BAD_SEQ_COUNT" -ne 0 ]; then
    fail "Found $BAD_SEQ_COUNT odd seq values (SeqLock invariant violated)"
fi
pass "All seq values are even (SeqLock invariant holds)"

# seq must be >= 2 (first valid complete write starts at old_seq=0 → seq=2)
BAD_SEQ2=$(grep -o '"seq":[0-9]*' "$TRACE_JSONL" \
    | awk -F: '{if ($2 < 2) print}' | wc -l || true)
if [ "$BAD_SEQ2" -ne 0 ]; then
    fail "Found $BAD_SEQ2 seq values < 2 (impossible for a completed write)"
fi
pass "All seq values >= 2"

# Multiple unique probe_ids must be present (many tasks were spawned)
UNIQUE_PROBES=$(grep -o '"probe_id":[0-9]*' "$TRACE_JSONL" \
    | sort -u | wc -l || true)
if [ "$UNIQUE_PROBES" -lt 2 ]; then
    fail "Only $UNIQUE_PROBES unique probe_id(s); expected > 1 for concurrent tasks"
fi
pass "JSONL has $UNIQUE_PROBES unique probe IDs (concurrent tasks confirmed)"

# ts must be > 0 (monotonic clock is always positive)
ZERO_TS=$(grep -o '"ts":0[^0-9]' "$TRACE_JSONL" | wc -l || true)
if [ "$ZERO_TS" -ne 0 ]; then
    fail "Found $ZERO_TS events with ts=0 (clock not working?)"
fi
pass "All ts values > 0 (nanosecond clock working)"

# ─── Phase 8: CSV export ──────────────────────────────────────────────────────
phase 8 "CSV export round-trip"
CSV_PATH="$OUTPUT_DIR/trace.csv"

"$TRACER_BIN" -export csv -in "$TRACE_JSONL" -csv-out "$CSV_PATH" \
    2>&1 | tee "$OUTPUT_DIR/csv_export.log"

if [ ! -f "$CSV_PATH" ]; then
    fail "CSV file not created: $CSV_PATH"
fi

CSV_LINES=$(grep -c . "$CSV_PATH" || true)
# header + data rows
EXPECTED_CSV=$((LINE_COUNT + 1))
if [ "$CSV_LINES" -ne "$EXPECTED_CSV" ]; then
    fail "CSV has $CSV_LINES lines, want $EXPECTED_CSV (header + $LINE_COUNT data)"
fi

CSV_HEADER=$(head -1 "$CSV_PATH")
if [ "$CSV_HEADER" != "probe_id,tid,addr,seq,is_active,ts" ]; then
    fail "CSV header = '$CSV_HEADER'"
fi
pass "CSV export: $LINE_COUNT data rows, header correct"

# Verify addr column is present and hex-prefixed
CSV_ADDR_SAMPLE=$(awk -F, 'NR==2{print $3}' "$CSV_PATH")
if [[ "$CSV_ADDR_SAMPLE" != 0x* ]]; then
    fail "CSV addr column row 1 = '$CSV_ADDR_SAMPLE' (expected 0x prefix)"
fi
pass "CSV addr column format: $CSV_ADDR_SAMPLE"

# ─── Phase 9: SQLite export (optional) ───────────────────────────────────────
phase 9 "SQLite export round-trip (optional)"
if command -v sqlite3 &>/dev/null; then
    DB_PATH="$OUTPUT_DIR/trace.sqlite"

    "$TRACER_BIN" -export sqlite -in "$TRACE_JSONL" -sqlite-out "$DB_PATH" \
        2>&1 | tee "$OUTPUT_DIR/sqlite_export.log"

    if [ ! -f "$DB_PATH" ]; then
        fail "SQLite db not created: $DB_PATH"
    fi

    DB_SIZE=$(wc -c < "$DB_PATH" || true)
    if [ "$DB_SIZE" -eq 0 ]; then
        fail "SQLite db is empty (0 bytes)"
    fi

    DB_ROWS=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM coro_trace_events;" || true)
    if [ "$DB_ROWS" -ne "$LINE_COUNT" ]; then
        fail "SQLite row count = $DB_ROWS, want $LINE_COUNT"
    fi
    pass "SQLite export: $DB_ROWS rows inserted"

    # Verify indexes
    INDEX_COUNT=$(sqlite3 "$DB_PATH" \
        "SELECT COUNT(*) FROM sqlite_master WHERE type='index';" || true)
    if [ "$INDEX_COUNT" -lt 3 ]; then
        fail "SQLite has only $INDEX_COUNT index(es), expected >= 3"
    fi
    pass "SQLite has $INDEX_COUNT indexes"

    # Quick sanity: both is_active values present
    ACTIVE_COUNT=$(sqlite3 "$DB_PATH" \
        "SELECT COUNT(*) FROM coro_trace_events WHERE is_active=1;" || true)
    SUSPEND_COUNT=$(sqlite3 "$DB_PATH" \
        "SELECT COUNT(*) FROM coro_trace_events WHERE is_active=0;" || true)
    if [ "$ACTIVE_COUNT" -eq 0 ] || [ "$SUSPEND_COUNT" -eq 0 ]; then
        fail "SQLite: active=$ACTIVE_COUNT suspend=$SUSPEND_COUNT (expected both > 0)"
    fi
    pass "SQLite: $ACTIVE_COUNT resume + $SUSPEND_COUNT suspend events"
else
    skip "sqlite3 not in PATH – SQLite export test skipped"
fi

# ─── Summary ──────────────────────────────────────────────────────────────────
echo
echo "════════════════════════════════════════"
echo -e "${GREEN}  ALL REQUIRED TESTS PASSED${NC}"
printf "  Passed: %d  |  Skipped: %d\n" "$PASS_COUNT" "$SKIP_COUNT"
echo "════════════════════════════════════════"
echo "  Output artefacts → $OUTPUT_DIR"
echo "    trace.jsonl  ($LINE_COUNT events)"
echo "    trace.csv"
[ -f "$OUTPUT_DIR/trace.sqlite" ] && echo "    trace.sqlite"
echo "════════════════════════════════════════"
