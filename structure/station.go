package structure

import (
	"sync/atomic"
)

// GlobalHeader 对应共享内存的起始 64 字节
// 用于跨进程的全局状态协商
type GlobalHeader struct {
	MagicNum       uint64   // 0x434F524F54524352
	Version        uint32   // 1
	MaxStations    uint32   // 最大容量
	AllocatedCount uint32   // 已分配数量 (由 C++/Rust 探针原子递增)
	TracerSleeping uint32   // 0: 活跃, 1: 睡眠 (用于智能唤醒)
	_              [40]byte // 凑齐 64 字节对齐
}

// Epoch 严格占 64 字节，匹配一个典型的 CPU Cache Line
type Epoch struct {
	Timestamp uint64   // 0x00
	TID       uint64   // 0x08
	Addr      uint64   // 0x10
	Seq       uint64   // 0x18 (必须通过 atomic 访问)
	Reserved  [31]byte // 0x20
	IsActive  bool     // 0x3F
}

// StationData 严格占 1024 字节，对应 cTP 协议中的 Station Block
// 注意：里面没有任何 Go Channel 或指针！完全兼容 C/C++ 内存布局
type StationData struct {
	Header struct {
		ProbeID uint64   // 0x00
		BirthTS uint64   // 0x08
		IsDead  bool     // 0x10
		_       [47]byte // 0x11 - 填充凑满 64 字节
	} // 占用 64 Bytes

	Slots    [8]Epoch  // 占用 512 Bytes (偏移 0x40 开始)
	Flexible [440]byte // 占用 448 Bytes (偏移 0x240 开始，对齐到 1KB)
}

// Harvest 执行一次无锁扫描，返回本次收集到的数据条数
// 注意：现在它不阻塞，也不负责 Flush。
func (s *StationData) Harvest(lastSeenSeqs *[8]uint64, sw *StationWriter) int {
	harvestedCount := 0
	for i := 0; i < 8; i++ {
		slot := &s.Slots[i]

		// 1. 原子读取 Seq 快照 (Acquire屏障)
		currentSeq := atomic.LoadUint64(&slot.Seq)

		if currentSeq > lastSeenSeqs[i] {
			// 2. 将快照传入写入函数，保证严格一致性
			sw.WriteSlot(s, i, currentSeq)

			lastSeenSeqs[i] = currentSeq
			harvestedCount++
		}
	}
	return harvestedCount
}
