package structure

import (
	"bufio"
	"os"
	"strconv"
)

// 快速十六进制转换表 (保持你的优秀设计)
const hexChars = "0123456789abcdef"

func appendHex(dst []byte, v uint64) []byte {
	dst = append(dst, '0', 'x')
	for i := 15; i >= 0; i-- {
		dst = append(dst, hexChars[(v>>(uint(i)*4))&0xf])
	}
	return dst
}

// MarshalSlotJSONL
// 改动 1: 接收者改为 StationData
// 改动 2: 强行传入 observedSeq，彻底消除二次读取造成的脏读
func (s *StationData) MarshalSlotJSONL(buf []byte, i int, observedSeq uint64) []byte {
	// 注意：这里的 s.Slots[i] 内存时刻在被 C++ 探针无锁并发修改！
	slot := &s.Slots[i]

	buf = append(buf, `{"probe_id":`...)
	buf = strconv.AppendUint(buf, s.Header.ProbeID, 10)

	buf = append(buf, `,"tid":`...)
	buf = strconv.AppendUint(buf, slot.TID, 10)

	buf = append(buf, `,"addr":"`...)
	buf = appendHex(buf, slot.Addr)

	buf = append(buf, `","seq":`...)
	// 🔴 关键安全修复：绝对不能读 slot.Seq，必须用外层传入的快照
	buf = strconv.AppendUint(buf, observedSeq, 10)

	buf = append(buf, `,"is_active":`...)
	if slot.IsActive {
		buf = append(buf, "true"...)
	} else {
		buf = append(buf, "false"...)
	}

	buf = append(buf, `,"ts":`...)
	buf = strconv.AppendUint(buf, slot.Timestamp, 10)

	// 关于 Flexible 的转义：
	// 如果你打算存 C++ 的局部变量快照等二进制数据，强烈建议用 hex 编码或 base64
	// buf = append(buf, `,"flex":"`...)
	// buf = 你的Hex编码函数(buf, s.Flexible[:有效长度])
	buf = append(buf, "}\n"...)

	return buf
}

// StationWriter 不再需要加锁！
// 在 cTP 协议下，整个系统只会有一个全局监听 Goroutine 操作它。
type StationWriter struct {
	file   *os.File
	writer *bufio.Writer
	line   []byte
}

func NewStationWriter(filename string) (*StationWriter, error) {
	// O_APPEND 配合 128KB 缓冲，能把磁盘 I/O 压榨到极限
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &StationWriter{
		file:   f,
		writer: bufio.NewWriterSize(f, 128*1024),
		line:   make([]byte, 0, 2048),
	}, nil
}

// WriteSlot
// 改动 3: 接收 StationData 和 observedSeq
func (sw *StationWriter) WriteSlot(s *StationData, slotIdx int, observedSeq uint64) error {
	sw.line = s.MarshalSlotJSONL(sw.line[:0], slotIdx, observedSeq)
	_, err := sw.writer.Write(sw.line)
	return err
}

func (sw *StationWriter) Flush() error {
	return sw.writer.Flush()
}

func (sw *StationWriter) Close() error {
	sw.Flush()
	return sw.file.Close()
}
