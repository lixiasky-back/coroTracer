package engine

import (
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// ─── Helpers ──────────────────────────────────────────────────────────────────

func tempPaths(t *testing.T) (shmPath, sockPath, logPath string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "engine_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	shmPath = dir + "/test.shm"
	sockPath = dir + "/test.sock"
	logPath = dir + "/test.jsonl"
	cleanup = func() { os.RemoveAll(dir) }
	return
}

// newEngine creates a TracerEngine with n stations and registers cleanup.
func newEngine(t *testing.T, n uint32) (*TracerEngine, string) {
	t.Helper()
	shm, sock, log, cleanup := tempPaths(t)
	t.Cleanup(cleanup)
	eng, err := NewTracerEngine(n, shm, sock, log)
	if err != nil {
		t.Fatalf("NewTracerEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	return eng, log
}

// ─── Constants ────────────────────────────────────────────────────────────────

func TestHeaderAndStationSizeConstants(t *testing.T) {
	if HeaderSize != 1024 {
		t.Errorf("HeaderSize = %d, want 1024", HeaderSize)
	}
	if StationSize != 1024 {
		t.Errorf("StationSize = %d, want 1024", StationSize)
	}
}

// ─── NewTracerEngine ──────────────────────────────────────────────────────────

func TestNewTracerEngineHeaderMagic(t *testing.T) {
	eng, _ := newEngine(t, 16)
	const wantMagic = uint64(0x434F524F54524352)
	if eng.header.MagicNum != wantMagic {
		t.Errorf("magic = 0x%x, want 0x%x", eng.header.MagicNum, wantMagic)
	}
}

func TestNewTracerEngineHeaderVersion(t *testing.T) {
	eng, _ := newEngine(t, 8)
	if eng.header.Version != 1 {
		t.Errorf("version = %d, want 1", eng.header.Version)
	}
}

func TestNewTracerEngineHeaderMaxStations(t *testing.T) {
	const n = uint32(32)
	eng, _ := newEngine(t, n)
	if eng.header.MaxStations != n {
		t.Errorf("max_stations = %d, want %d", eng.header.MaxStations, n)
	}
}

func TestNewTracerEngineHeaderAllocatedZero(t *testing.T) {
	eng, _ := newEngine(t, 16)
	got := atomic.LoadUint32(&eng.header.AllocatedCount)
	if got != 0 {
		t.Errorf("allocated_count = %d at init, want 0", got)
	}
}

func TestNewTracerEngineHeaderTracerSleepingZero(t *testing.T) {
	eng, _ := newEngine(t, 8)
	got := atomic.LoadUint32(&eng.header.TracerSleeping)
	if got != 0 {
		t.Errorf("tracer_sleeping = %d at init, want 0", got)
	}
}

func TestNewTracerEngineCreatesFiles(t *testing.T) {
	shm, sock, log, cleanup := tempPaths(t)
	t.Cleanup(cleanup)

	eng, err := NewTracerEngine(8, shm, sock, log)
	if err != nil {
		t.Fatalf("NewTracerEngine: %v", err)
	}
	t.Cleanup(eng.Close)

	for _, path := range []string{shm, log} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected file at %s", path)
		}
	}
}

func TestNewTracerEngineShmFileSize(t *testing.T) {
	shm, sock, log, cleanup := tempPaths(t)
	t.Cleanup(cleanup)

	const n = uint32(64)
	eng, err := NewTracerEngine(n, shm, sock, log)
	if err != nil {
		t.Fatalf("NewTracerEngine: %v", err)
	}
	t.Cleanup(eng.Close)

	info, err := os.Stat(shm)
	if err != nil {
		t.Fatalf("stat shm: %v", err)
	}
	want := int64(HeaderSize + int(n)*StationSize)
	if info.Size() != want {
		t.Errorf("shm size = %d, want %d", info.Size(), want)
	}
}

func TestNewTracerEngineStationsAndLastSeenLen(t *testing.T) {
	const n = uint32(48)
	eng, _ := newEngine(t, n)

	if uint32(len(eng.stations)) != n {
		t.Errorf("stations len = %d, want %d", len(eng.stations), n)
	}
	if uint32(len(eng.lastSeen)) != n {
		t.Errorf("lastSeen len = %d, want %d", len(eng.lastSeen), n)
	}
}

func TestNewTracerEngineMinimalStations(t *testing.T) {
	eng, _ := newEngine(t, 1)
	if len(eng.stations) != 1 {
		t.Errorf("minimal stations = %d, want 1", len(eng.stations))
	}
}

// ─── Close ────────────────────────────────────────────────────────────────────

func TestCloseIsIdempotent(t *testing.T) {
	shm, sock, log, cleanup := tempPaths(t)
	t.Cleanup(cleanup)

	eng, err := NewTracerEngine(4, shm, sock, log)
	if err != nil {
		t.Fatalf("NewTracerEngine: %v", err)
	}
	eng.Close()
	eng.Close() // must not panic
}

// ─── doScan ───────────────────────────────────────────────────────────────────

func TestDoScanEmptyReturnsZero(t *testing.T) {
	eng, _ := newEngine(t, 8)
	if got := eng.doScan(); got != 0 {
		t.Errorf("empty doScan = %d, want 0", got)
	}
}

func TestDoScanWithOneEvent(t *testing.T) {
	eng, log := newEngine(t, 8)

	// Simulate a coroutine registering and writing one SeqLock event.
	atomic.StoreUint32(&eng.header.AllocatedCount, 1)
	eng.stations[0].Header.ProbeID = 77

	slot := &eng.stations[0].Slots[0]
	old := atomic.LoadUint64(&slot.Seq)
	atomic.StoreUint64(&slot.Seq, old+1) // lock
	slot.TID = 42
	slot.Addr = 0x1234
	slot.IsActive = true
	slot.Timestamp = 999
	atomic.StoreUint64(&slot.Seq, old+2) // unlock

	if got := eng.doScan(); got != 1 {
		t.Errorf("doScan with event = %d, want 1", got)
	}

	// Second scan: nothing new
	if got := eng.doScan(); got != 0 {
		t.Errorf("second doScan = %d, want 0", got)
	}

	// Verify JSONL was written
	eng.writer.Flush()
	data, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Errorf("expected 1 JSONL line, got %d", len(lines))
	}
	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Errorf("invalid JSONL: %v", err)
	}
	if rec["probe_id"] != float64(77) {
		t.Errorf("probe_id = %v, want 77", rec["probe_id"])
	}
}

func TestDoScanMultipleStations(t *testing.T) {
	eng, _ := newEngine(t, 8)

	atomic.StoreUint32(&eng.header.AllocatedCount, 3)

	for i := 0; i < 3; i++ {
		slot := &eng.stations[i].Slots[0]
		old := atomic.LoadUint64(&slot.Seq)
		atomic.StoreUint64(&slot.Seq, old+1)
		slot.TID = uint64(i + 1)
		slot.Addr = uint64(i * 0x100)
		slot.IsActive = true
		slot.Timestamp = uint64(i * 100)
		atomic.StoreUint64(&slot.Seq, old+2)
	}

	if got := eng.doScan(); got != 3 {
		t.Errorf("doScan 3 stations = %d, want 3", got)
	}
}

func TestDoScanClampsToMaxStations(t *testing.T) {
	const maxN = uint32(4)
	eng, _ := newEngine(t, maxN)

	// AllocatedCount is much larger than maxStations
	atomic.StoreUint32(&eng.header.AllocatedCount, maxN+100)

	// doScan must not access stations[maxN..] (out of bounds) - it would panic
	got := eng.doScan()
	if got != 0 {
		t.Errorf("empty stations but over-count: doScan = %d, want 0", got)
	}
}

func TestDoScanSkipsOddSeq(t *testing.T) {
	eng, _ := newEngine(t, 4)
	atomic.StoreUint32(&eng.header.AllocatedCount, 1)

	// Force odd seq
	atomic.StoreUint64(&eng.stations[0].Slots[0].Seq, 1)

	if got := eng.doScan(); got != 0 {
		t.Errorf("odd seq: doScan = %d, want 0", got)
	}
}

func TestDoScanAllEightSlotsPerStation(t *testing.T) {
	eng, _ := newEngine(t, 4)
	atomic.StoreUint32(&eng.header.AllocatedCount, 1)

	for i := 0; i < 8; i++ {
		slot := &eng.stations[0].Slots[i]
		old := atomic.LoadUint64(&slot.Seq)
		atomic.StoreUint64(&slot.Seq, old+1)
		slot.TID = uint64(i)
		slot.Addr = uint64(i * 8)
		slot.IsActive = i%2 == 0
		slot.Timestamp = uint64(i * 1000)
		atomic.StoreUint64(&slot.Seq, old+2)
	}

	if got := eng.doScan(); got != 8 {
		t.Errorf("8 slots per station: doScan = %d, want 8", got)
	}
}

// ─── maxStations field ────────────────────────────────────────────────────────

func TestMaxStationsFieldStored(t *testing.T) {
	const n = uint32(128)
	eng, _ := newEngine(t, n)
	if eng.maxStations != n {
		t.Errorf("maxStations = %d, want %d", eng.maxStations, n)
	}
}
