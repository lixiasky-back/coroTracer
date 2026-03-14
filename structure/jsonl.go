package structure

import (
	"bufio"
	"os"
	"strconv"
)

const hexChars = "0123456789abcdef"

func appendHex(dst []byte, v uint64) []byte {
	dst = append(dst, '0', 'x')
	for i := 15; i >= 0; i-- {
		dst = append(dst, hexChars[(v>>(uint(i)*4))&0xf])
	}
	return dst
}

// MarshalSlotJSONL
// Change 1: Modify the receiver to StationData
// Change 2: Force pass observedSeq to completely eliminate dirty reads caused by secondary reads
func (s *StationData) marshalSafeSlotJSONL(buf []byte, safeSeq, tid, addr uint64, isActive bool, ts uint64) []byte {
	buf = append(buf, `{"probe_id":`...)
	buf = strconv.AppendUint(buf, s.Header.ProbeID, 10)

	buf = append(buf, `,"tid":`...)
	buf = strconv.AppendUint(buf, tid, 10)

	buf = append(buf, `,"addr":"`...)
	buf = appendHex(buf, addr)

	buf = append(buf, `","seq":`...)
	buf = strconv.AppendUint(buf, safeSeq, 10)

	buf = append(buf, `,"is_active":`...)
	if isActive {
		buf = append(buf, "true"...)
	} else {
		buf = append(buf, "false"...)
	}

	buf = append(buf, `,"ts":`...)
	buf = strconv.AppendUint(buf, ts, 10)

	buf = append(buf, "}\n"...)

	return buf
}

// StationWriter no longer needs to be locked!
// Under the cTP protocol, there will only be one global listening Goroutine operating it in the entire system.
type StationWriter struct {
	file   *os.File
	writer *bufio.Writer
	line   []byte
}

func NewStationWriter(filename string) (*StationWriter, error) {
	// O_APPEND combined with 128KB buffering can squeeze disk I/O to the limit
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
// Change 3: Receive StationData and observedSeq
func (sw *StationWriter) WriteSafeSlot(s *StationData, safeSeq, tid, addr uint64, isActive bool, ts uint64) error {
	sw.line = s.marshalSafeSlotJSONL(sw.line[:0], safeSeq, tid, addr, isActive, ts)
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
