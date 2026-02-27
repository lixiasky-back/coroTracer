package engine

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"

	"coroTracer/structure"
)

const (
	// 🔴 核心修复：必须与 structure.GlobalHeader 保持绝对一致，占用完整的 1KB！
	HeaderSize  = 1024
	StationSize = 1024
)

type TracerEngine struct {
	shmFile  *os.File
	mmapData []byte

	// 内存映射指针（黑魔法零拷贝）
	header   *structure.GlobalHeader
	stations []structure.StationData

	writer   *structure.StationWriter
	listener net.Listener

	maxStations uint32 // 动态容量
	lastSeen    [][8]uint64
}

// NewTracerEngine 初始化共享内存、Socket 和日志文件
func NewTracerEngine(stationCount uint32, shmPath, sockPath, logPath string) (*TracerEngine, error) {
	// 动态计算总内存大小
	memSize := HeaderSize + (int(stationCount) * StationSize)

	os.Remove(shmPath)
	// 1. 创建共享内存文件并截断到精确的 memSize
	f, err := os.OpenFile(shmPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(memSize)); err != nil {
		return nil, err
	}

	// 2. Mmap 映射
	mmapData, err := syscall.Mmap(int(f.Fd()), 0, memSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}

	// 3. 结构体强转 (GlobalHeader 现在是 1024 字节)
	header := (*structure.GlobalHeader)(unsafe.Pointer(&mmapData[0]))
	header.MagicNum = 0x434F524F54524352
	header.Version = 1
	header.MaxStations = stationCount
	atomic.StoreUint32(&header.AllocatedCount, 0)
	atomic.StoreUint32(&header.TracerSleeping, 0)

	// 🔴 动态切片映射：完美越过 1024 字节的 Header，精确踩中 Station[0]
	stations := unsafe.Slice((*structure.StationData)(unsafe.Pointer(&mmapData[HeaderSize])), stationCount)

	// 4. 创建 UDS Socket
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen uds failed: %v", err)
	}

	// 5. 初始化日志写入器
	writer, err := structure.NewStationWriter(logPath)
	if err != nil {
		return nil, err
	}

	return &TracerEngine{
		shmFile:     f,
		mmapData:    mmapData,
		header:      header,
		stations:    stations,
		writer:      writer,
		listener:    listener,
		maxStations: stationCount,
		lastSeen:    make([][8]uint64, stationCount),
	}, nil
}

func (e *TracerEngine) Run() error {
	fmt.Println("Tracer Engine listening on UDS...")
	wakeBuf := make([]byte, 1024)

	for {
		conn, err := e.listener.Accept()
		if err != nil {
			fmt.Printf("Accept error: %v\n", err)
			continue
		}
		fmt.Println("Tracee connected! Entering hot loop.")

		e.hotHarvestLoop(conn, wakeBuf)

		fmt.Println("Tracee disconnected. Waiting for next connection...")
		conn.Close()
	}
}

func (e *TracerEngine) doScan() int {
	totalHarvested := 0
	allocated := atomic.LoadUint32(&e.header.AllocatedCount)

	// 🔴 逻辑修复：使用实例自己的 maxStations，而不是之前写死的常量
	if allocated > e.maxStations {
		allocated = e.maxStations
	}

	for i := uint32(0); i < allocated; i++ {
		totalHarvested += e.stations[i].Harvest(&e.lastSeen[i], e.writer)
	}
	return totalHarvested
}

func (e *TracerEngine) hotHarvestLoop(conn net.Conn, wakeBuf []byte) {
	for {
		harvested := e.doScan()

		if harvested > 0 {
			continue
		}

		e.writer.Flush()
		atomic.StoreUint32(&e.header.TracerSleeping, 1)

		if e.doScan() > 0 {
			atomic.StoreUint32(&e.header.TracerSleeping, 0)
			continue
		}

		n, err := conn.Read(wakeBuf)
		if err != nil || n == 0 {
			atomic.StoreUint32(&e.header.TracerSleeping, 0)
			return
		}

		atomic.StoreUint32(&e.header.TracerSleeping, 0)
	}
}

func (e *TracerEngine) Close() {
	if e.writer != nil {
		e.writer.Close()
	}
	if e.listener != nil {
		e.listener.Close()
	}
	if e.mmapData != nil {
		syscall.Munmap(e.mmapData)
	}
	if e.shmFile != nil {
		e.shmFile.Close()
	}
}
