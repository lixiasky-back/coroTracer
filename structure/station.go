package structure

import (
	"sync/atomic"
)

// GlobalHeader 强制占用完整的 1024 字节 (1KB)
// 这样可以确保紧跟在它后面的 StationData 绝对是 1024 字节对齐的
type GlobalHeader struct {
	MagicNum       uint64     // 0x00
	Version        uint32     // 0x08
	MaxStations    uint32     // 0x0C
	AllocatedCount uint32     // 0x10
	TracerSleeping uint32     // 0x14
	_              [1004]byte // 🔴 1024 - 20 = 1004。硬填充，拒绝 C++ 隐式 Padding
}

// Epoch 严格占 64 字节，匹配 CPU Cache Line
type Epoch struct {
	Timestamp uint64   // 0x00
	TID       uint64   // 0x08
	Addr      uint64   // 0x10
	Seq       uint64   // 0x18
	Reserved  [31]byte // 0x20
	IsActive  bool     // 0x3F
}

// StationData 严格占 1024 字节
type StationData struct {
	Header struct {
		ProbeID uint64   // 0x00
		BirthTS uint64   // 0x08
		IsDead  bool     // 0x10
		_       [47]byte // 0x11 - 填充凑满 64 字节
	} // 占用 64 Bytes

	Slots [8]Epoch // 占用 512 Bytes (8 * 64)

	// 🔴 修复数学算错的 Bug：64 + 512 + 448 = 1024 Bytes
	Flexible [448]byte
}

// Harvest 执行一次无锁扫描，返回本次收集到的数据条数
func (s *StationData) Harvest(lastSeenSeqs *[8]uint64, sw *StationWriter) int {
	harvestedCount := 0
	for i := 0; i < 8; i++ {
		slot := &s.Slots[i]

		// 1. 原子读取 Seq 快照
		currentSeq := atomic.LoadUint64(&slot.Seq)

		if currentSeq > lastSeenSeqs[i] {
			// 2. 将快照传入写入函数
			sw.WriteSlot(s, i, currentSeq)

			lastSeenSeqs[i] = currentSeq
			harvestedCount++
		}
	}
	return harvestedCount
}
