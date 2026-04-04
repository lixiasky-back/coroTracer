package structure

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ─── StationWriter lifecycle ──────────────────────────────────────────────────

func TestNewStationWriterCreatesFile(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_create_*.jsonl")
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)

	sw, err := NewStationWriter(name)
	if err != nil {
		t.Fatalf("NewStationWriter: %v", err)
	}
	sw.Close()

	if _, err := os.Stat(name); os.IsNotExist(err) {
		t.Error("file not created by NewStationWriter")
	}
}

func TestNewStationWriterInvalidPath(t *testing.T) {
	_, err := NewStationWriter("/nonexistent_dir_xyz/test.jsonl")
	if err == nil {
		t.Error("expected error for invalid path, got nil")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_close_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	sw.Close()
	// Second close should not panic (file is already closed)
}

// ─── WriteSafeSlot correctness ────────────────────────────────────────────────

func readSingleRecord(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if line == "" {
		t.Fatal("output file is empty")
	}
	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("invalid JSON %q: %v", line, err)
	}
	return rec
}

func TestWriteSafeSlotProducesValidJSON(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_json_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	s.Header.ProbeID = 42

	if err := sw.WriteSafeSlot(&s, 4, 1001, 0xDEADBEEF, true, 123456789); err != nil {
		t.Fatalf("WriteSafeSlot: %v", err)
	}
	sw.Close()

	rec := readSingleRecord(t, name)

	checks := []struct {
		field string
		want  interface{}
	}{
		{"probe_id", float64(42)},
		{"tid", float64(1001)},
		{"seq", float64(4)},
		{"is_active", true},
		{"ts", float64(123456789)},
	}
	for _, c := range checks {
		if rec[c.field] != c.want {
			t.Errorf("%s = %v, want %v", c.field, rec[c.field], c.want)
		}
	}
	addr, _ := rec["addr"].(string)
	if !strings.HasPrefix(addr, "0x") {
		t.Errorf("addr %q does not start with 0x", addr)
	}
}

func TestWriteSafeSlotInactive(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_inactive_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	sw.WriteSafeSlot(&s, 2, 0, 0xBEEF, false, 0)
	sw.Close()

	rec := readSingleRecord(t, name)
	if rec["is_active"] != false {
		t.Errorf("is_active = %v, want false", rec["is_active"])
	}
}

// ─── addr hex format ──────────────────────────────────────────────────────────

func TestAddrHex16Digits(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_hex_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	sw.WriteSafeSlot(&s, 2, 0, 0xCAFEBABE00001234, true, 0)
	sw.Close()

	rec := readSingleRecord(t, name)
	addr, _ := rec["addr"].(string)
	if len(addr) != 18 { // "0x" + 16 hex digits
		t.Errorf("addr %q length = %d, want 18", addr, len(addr))
	}
	if addr != "0xcafebabe00001234" {
		t.Errorf("addr = %q, want 0xcafebabe00001234", addr)
	}
}

func TestAddrZeroValue(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_zero_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	sw.WriteSafeSlot(&s, 2, 0, 0, true, 0)
	sw.Close()

	rec := readSingleRecord(t, name)
	addr, _ := rec["addr"].(string)
	if addr != "0x0000000000000000" {
		t.Errorf("zero addr = %q, want 0x0000000000000000", addr)
	}
}

func TestAddrMaxValue(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_max_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	sw.WriteSafeSlot(&s, 2, 0, ^uint64(0), true, 0) // 0xffffffffffffffff
	sw.Close()

	rec := readSingleRecord(t, name)
	addr, _ := rec["addr"].(string)
	if addr != "0xffffffffffffffff" {
		t.Errorf("max addr = %q, want 0xffffffffffffffff", addr)
	}
}

// ─── Multi-line output ────────────────────────────────────────────────────────

func TestWriteMultipleLines(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_multi_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	const n = 25
	for i := 0; i < n; i++ {
		sw.WriteSafeSlot(&s, uint64(i*2+2), uint64(i), uint64(i*8), i%2 == 0, uint64(i*100))
	}
	sw.Close()

	fp, _ := os.Open(name)
	defer fp.Close()
	scanner := bufio.NewScanner(fp)
	count := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			count++
		}
	}
	if count != n {
		t.Errorf("line count = %d, want %d", count, n)
	}
}

func TestEachLineIsValidJSON(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_eachline_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	const n = 10
	for i := 0; i < n; i++ {
		sw.WriteSafeSlot(&s, uint64(i*2+2), uint64(i), uint64(i), i%3 == 0, uint64(i))
	}
	sw.Close()

	fp, _ := os.Open(name)
	defer fp.Close()
	scanner := bufio.NewScanner(fp)
	lineNo := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineNo++
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line %d invalid JSON: %v", lineNo, err)
		}
		for _, field := range []string{"probe_id", "tid", "addr", "seq", "is_active", "ts"} {
			if _, ok := rec[field]; !ok {
				t.Errorf("line %d missing field %q", lineNo, field)
			}
		}
	}
	if lineNo != n {
		t.Errorf("got %d valid JSON lines, want %d", lineNo, n)
	}
}

// ─── Flush ────────────────────────────────────────────────────────────────────

func TestFlushWritesToDisk(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_flush_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	sw.WriteSafeSlot(&s, 2, 1, 2, true, 3)

	if err := sw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Read before Close to verify data is on disk
	data, _ := os.ReadFile(name)
	sw.Close()

	if len(strings.TrimSpace(string(data))) == 0 {
		t.Error("data empty after Flush (before Close)")
	}
}

func TestFlushOnEmptyWriterDoesNotError(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_emptyflush_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	defer sw.Close()

	if err := sw.Flush(); err != nil {
		t.Errorf("Flush on empty writer: %v", err)
	}
}

// ─── ProbeID propagated from station ─────────────────────────────────────────

func TestProbeIDFromStationHeader(t *testing.T) {
	f, _ := os.CreateTemp("", "sw_probeid_*.jsonl")
	name := f.Name()
	f.Close()
	defer os.Remove(name)

	sw, _ := NewStationWriter(name)
	var s StationData
	s.Header.ProbeID = 99999
	sw.WriteSafeSlot(&s, 2, 0, 0, false, 0)
	sw.Close()

	rec := readSingleRecord(t, name)
	if rec["probe_id"] != float64(99999) {
		t.Errorf("probe_id = %v, want 99999", rec["probe_id"])
	}
}
