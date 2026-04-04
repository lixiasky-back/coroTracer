package structure

import (
	"os"
	"sync/atomic"
	"testing"
	"unsafe"
)

// ─── Memory layout ────────────────────────────────────────────────────────────

func TestGlobalHeaderSize(t *testing.T) {
	if got := unsafe.Sizeof(GlobalHeader{}); got != 1024 {
		t.Errorf("GlobalHeader size = %d, want 1024", got)
	}
}

func TestEpochSize(t *testing.T) {
	if got := unsafe.Sizeof(Epoch{}); got != 64 {
		t.Errorf("Epoch size = %d, want 64", got)
	}
}

func TestStationDataSize(t *testing.T) {
	if got := unsafe.Sizeof(StationData{}); got != 1024 {
		t.Errorf("StationData size = %d, want 1024", got)
	}
}

func TestGlobalHeaderFieldOffsets(t *testing.T) {
	var h GlobalHeader
	base := uintptr(unsafe.Pointer(&h))

	cases := []struct {
		name   string
		got    uintptr
		wantOff uintptr
	}{
		{"MagicNum", uintptr(unsafe.Pointer(&h.MagicNum)) - base, 0x00},
		{"Version", uintptr(unsafe.Pointer(&h.Version)) - base, 0x08},
		{"MaxStations", uintptr(unsafe.Pointer(&h.MaxStations)) - base, 0x0C},
		{"AllocatedCount", uintptr(unsafe.Pointer(&h.AllocatedCount)) - base, 0x10},
		{"TracerSleeping", uintptr(unsafe.Pointer(&h.TracerSleeping)) - base, 0x14},
	}
	for _, c := range cases {
		if c.got != c.wantOff {
			t.Errorf("%s offset = 0x%02x, want 0x%02x", c.name, c.got, c.wantOff)
		}
	}
}

func TestEpochFieldOffsets(t *testing.T) {
	var e Epoch
	base := uintptr(unsafe.Pointer(&e))
	cases := []struct {
		name    string
		got     uintptr
		wantOff uintptr
	}{
		{"Timestamp", uintptr(unsafe.Pointer(&e.Timestamp)) - base, 0x00},
		{"TID", uintptr(unsafe.Pointer(&e.TID)) - base, 0x08},
		{"Addr", uintptr(unsafe.Pointer(&e.Addr)) - base, 0x10},
		{"Seq", uintptr(unsafe.Pointer(&e.Seq)) - base, 0x18},
		{"IsActive", uintptr(unsafe.Pointer(&e.IsActive)) - base, 0x3F},
	}
	for _, c := range cases {
		if c.got != c.wantOff {
			t.Errorf("Epoch.%s offset = 0x%02x, want 0x%02x", c.name, c.got, c.wantOff)
		}
	}
}

func TestStationDataSlotCount(t *testing.T) {
	var s StationData
	if len(s.Slots) != 8 {
		t.Errorf("Slots count = %d, want 8", len(s.Slots))
	}
}

// ─── SeqLock helpers ──────────────────────────────────────────────────────────

// simulateSeqLockWrite performs an atomic SeqLock write into slot, exactly
// mirroring what the C++ / Rust SDK does.
func simulateSeqLockWrite(slot *Epoch, tid, addr uint64, isActive bool, ts uint64) {
	old := atomic.LoadUint64(&slot.Seq)
	atomic.StoreUint64(&slot.Seq, old+1) // Step A: odd = writing
	slot.TID = tid
	slot.Addr = addr
	slot.IsActive = isActive
	slot.Timestamp = ts
	atomic.StoreUint64(&slot.Seq, old+2) // Step C: even = done
}

func newTestWriter(t *testing.T) (*StationWriter, func()) {
	t.Helper()
	f, err := os.CreateTemp("", "station_test_*.jsonl")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	name := f.Name()
	f.Close()
	sw, err := NewStationWriter(name)
	if err != nil {
		os.Remove(name)
		t.Fatalf("NewStationWriter: %v", err)
	}
	return sw, func() { sw.Close(); os.Remove(name) }
}

// ─── Harvest tests ────────────────────────────────────────────────────────────

func TestHarvestEmptyStation(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64
	if got := s.Harvest(&lastSeen, sw); got != 0 {
		t.Errorf("empty station: Harvest = %d, want 0", got)
	}
}

func TestHarvestSingleWrite(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	s.Header.ProbeID = 42
	var lastSeen [8]uint64

	simulateSeqLockWrite(&s.Slots[0], 1001, 0xDEADBEEF, true, 999)

	if got := s.Harvest(&lastSeen, sw); got != 1 {
		t.Errorf("single write: Harvest = %d, want 1", got)
	}
	if lastSeen[0] == 0 {
		t.Error("lastSeen[0] not updated after harvest")
	}
}

func TestHarvestDoesNotRepeatSameSeq(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64
	simulateSeqLockWrite(&s.Slots[0], 1001, 0xABCD, false, 100)
	s.Harvest(&lastSeen, sw)

	if got := s.Harvest(&lastSeen, sw); got != 0 {
		t.Errorf("repeat harvest: got %d, want 0", got)
	}
}

func TestHarvestSkipsOddSeq(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64
	// Force odd seq (C++ is mid-write)
	atomic.StoreUint64(&s.Slots[0].Seq, 3)

	if got := s.Harvest(&lastSeen, sw); got != 0 {
		t.Errorf("odd seq: Harvest = %d, want 0", got)
	}
}

func TestHarvestAllEightSlots(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	s.Header.ProbeID = 7
	var lastSeen [8]uint64

	for i := 0; i < 8; i++ {
		simulateSeqLockWrite(&s.Slots[i], uint64(100+i), uint64(i*16), i%2 == 0, uint64(i*1000))
	}

	if got := s.Harvest(&lastSeen, sw); got != 8 {
		t.Errorf("all slots: Harvest = %d, want 8", got)
	}
	for i := 0; i < 8; i++ {
		if lastSeen[i] == 0 {
			t.Errorf("lastSeen[%d] not updated", i)
		}
	}
}

func TestHarvestPartialSlots(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64
	// Only slots 1, 3, 5 have data
	simulateSeqLockWrite(&s.Slots[1], 101, 0x11, true, 1)
	simulateSeqLockWrite(&s.Slots[3], 103, 0x33, false, 3)
	simulateSeqLockWrite(&s.Slots[5], 105, 0x55, true, 5)

	if got := s.Harvest(&lastSeen, sw); got != 3 {
		t.Errorf("partial slots: Harvest = %d, want 3", got)
	}
}

func TestHarvestRingBufferWrapAround(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64

	// Round 1: fill all 8 slots
	for i := 0; i < 8; i++ {
		simulateSeqLockWrite(&s.Slots[i], uint64(i), uint64(i), true, uint64(i))
	}
	s.Harvest(&lastSeen, sw)

	// Round 2: overwrite slots 0 and 1 (ring wrap)
	simulateSeqLockWrite(&s.Slots[0], 200, 0xFF, false, 999)
	simulateSeqLockWrite(&s.Slots[1], 201, 0xFE, true, 998)

	if got := s.Harvest(&lastSeen, sw); got != 2 {
		t.Errorf("wrap-around: Harvest = %d, want 2", got)
	}
}

// A torn read is simulated by leaving seq odd after a "write" that never
// completes the unlock step. Harvest must discard such a slot.
func TestHarvestDiscardsTornRead(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64

	// seq=2 looks complete …
	atomic.StoreUint64(&s.Slots[0].Seq, 2)
	s.Slots[0].TID = 99
	// … but immediately change seq so seq1 != seq2 during the validation step.
	// We achieve this by leaving seq at an odd value > lastSeen. Harvest reads
	// seq1=odd and skips (odd check fires before payload copy).
	atomic.StoreUint64(&s.Slots[0].Seq, 3)

	if got := s.Harvest(&lastSeen, sw); got != 0 {
		t.Errorf("torn read guard: Harvest = %d, want 0", got)
	}
}

func TestHarvestIncreasesLastSeen(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	var lastSeen [8]uint64

	simulateSeqLockWrite(&s.Slots[2], 777, 0xCAFE, true, 42)
	s.Harvest(&lastSeen, sw)

	if lastSeen[2] == 0 {
		t.Error("lastSeen[2] still 0 after harvest")
	}

	seqBefore := lastSeen[2]
	simulateSeqLockWrite(&s.Slots[2], 888, 0xBEEF, false, 99)
	s.Harvest(&lastSeen, sw)

	if lastSeen[2] <= seqBefore {
		t.Errorf("lastSeen[2] not increased: %d -> %d", seqBefore, lastSeen[2])
	}
}

func TestHarvestZeroAddrAndInactive(t *testing.T) {
	sw, cleanup := newTestWriter(t)
	defer cleanup()

	var s StationData
	s.Header.ProbeID = 10
	var lastSeen [8]uint64

	// addr=0, is_active=true represents a resume event
	simulateSeqLockWrite(&s.Slots[0], 500, 0, true, 12345)

	if got := s.Harvest(&lastSeen, sw); got != 1 {
		t.Errorf("zero addr resume: Harvest = %d, want 1", got)
	}
}
