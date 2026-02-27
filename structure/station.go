package structure

import (
	"sync/atomic"
)

// GlobalHeader forcibly occupies a full 1024 bytes (1KB)
// This ensures that the StationData immediately following it is absolutely 1024-byte aligned
type GlobalHeader struct {
	MagicNum       uint64     // 0x00
	Version        uint32     // 0x08
	MaxStations    uint32     // 0x0C
	AllocatedCount uint32     // 0x10
	TracerSleeping uint32     // 0x14
	_              [1004]byte // 🔴 1024 - 20 = 1004. Hard padding, reject C++ implicit padding
}

// Epoch strictly occupies 64 bytes, matching the CPU Cache Line
type Epoch struct {
	Timestamp uint64   // 0x00
	TID       uint64   // 0x08
	Addr      uint64   // 0x10
	Seq       uint64   // 0x18
	Reserved  [31]byte // 0x20
	IsActive  bool     // 0x3F
}

// StationData strictly occupies 1024 bytes
type StationData struct {
	Header struct {
		ProbeID uint64   // 0x00
		BirthTS uint64   // 0x08
		IsDead  bool     // 0x10
		_       [47]byte // 0x11 - Pad to fill up to 64 bytes
	} // Occupy 64 Bytes

	Slots [8]Epoch // Occupy 512 Bytes (8 * 64)

	Flexible [448]byte
}

// Harvest performs a lock-free scan once and returns the number of data entries collected in this scan
func (s *StationData) Harvest(lastSeenSeqs *[8]uint64, sw *StationWriter) int {
	harvestedCount := 0
	for i := 0; i < 8; i++ {
		slot := &s.Slots[i]

		// 1. Atomically read the Seq snapshot
		currentSeq := atomic.LoadUint64(&slot.Seq)

		if currentSeq > lastSeenSeqs[i] {
			// 2. Pass the snapshot to the write function
			sw.WriteSlot(s, i, currentSeq)

			lastSeenSeqs[i] = currentSeq
			harvestedCount++
		}
	}
	return harvestedCount
}
