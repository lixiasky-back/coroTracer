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

// Harvest implements strict SeqLock for tear-free lock-free scanning
func (s *StationData) Harvest(lastSeenSeqs *[8]uint64, sw *StationWriter) int {
	harvestedCount := 0
	for i := 0; i < 8; i++ {
		slot := &s.Slots[i]

		// 🔵 Lean: go_scan (Step 1: Read pre-snapshot)
		// Use LoadUint64 to guarantee memory barrier semantics
		seq1 := atomic.LoadUint64(&slot.Seq)

		// Condition 1: Skip if no new data
		if seq1 <= lastSeenSeqs[i] {
			continue
		}
		// Condition 2: Skip if Seq is odd (C++ is writing, data unstable)
		if seq1%2 != 0 {
			continue
		}

		// 🔵 Lean: go_read (Step 2: Copy Payload quickly to local/registers)
		//Warning: C++ may wrap around and overwrite the slot memory at any time!
		//Here we simply copy field by field; reading garbled data is allowed because the next step provides a safety net.
		localTID := slot.TID
		localAddr := slot.Addr
		localIsActive := slot.IsActive
		localTS := slot.Timestamp

		// 🔵 Lean: go_validate (Step 3: Backstab Validation)
		// Use the memory barrier of LoadUint64 again to verify if C++ touched this slot during the copy operation.
		seq2 := atomic.LoadUint64(&slot.Seq)

		// Core defense: If Seq has changed, it means wrap-around or tearing occurred!
		// Corresponding to go_validate_fail in Lean, directly discard the dirty data just copied
		if seq1 != seq2 {
			continue
		}

		// 🟢 Validation passed! Corresponding to go_validate_pass in Lean
		// At this point, variables such as localTID are 100% from a complete, clean C++ write
		sw.WriteSafeSlot(s, seq1, localTID, localAddr, localIsActive, localTS)

		lastSeenSeqs[i] = seq1
		harvestedCount++
	}
	return harvestedCount
}
