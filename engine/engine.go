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
	HeaderSize  = 64
	StationSize = 1024
	MaxStations = 10000 // 预设支持 1 万个并发协程
	MemSize     = HeaderSize + (MaxStations * StationSize)
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
	// 记录每个 Station 的 8 个 Slot 读到了哪个 Seq
	lastSeen [][8]uint64
}

// NewTracerEngine 初始化共享内存、Socket 和日志文件
// NewTracerEngine 增加 stationCount 参数
func NewTracerEngine(stationCount uint32, shmPath, sockPath, logPath string) (*TracerEngine, error) {
	memSize := HeaderSize + (int(stationCount) * StationSize)

	// 1. 创建共享内存文件并截断到精确的 memSize
	f, err := os.OpenFile(shmPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(int64(memSize)); err != nil {
		return nil, err
	}

	// 2. Mmap 映射 (大小为动态的 memSize)
	mmapData, err := syscall.Mmap(int(f.Fd()), 0, memSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}

	// 3. 结构体强转
	header := (*structure.GlobalHeader)(unsafe.Pointer(&mmapData[0]))
	header.MagicNum = 0x434F524F54524352
	header.Version = 1
	header.MaxStations = stationCount // 写入全局头部，C++ 端可以通过这个防越界
	atomic.StoreUint32(&header.AllocatedCount, 0)
	atomic.StoreUint32(&header.TracerSleeping, 0)

	// 动态切片映射
	stations := unsafe.Slice((*structure.StationData)(unsafe.Pointer(&mmapData[HeaderSize])), stationCount)

	// 4. 创建 UDS Socket 用于极速唤醒
	os.Remove(sockPath) // 清理历史残留
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
		lastSeen:    make([][8]uint64, stationCount), // 动态初始化上一次看到的 seq 记录
	}, nil
}

// Run 启动主事件循环，支持被测程序反复重启连接
func (e *TracerEngine) Run() error {
	fmt.Println("Tracer Engine listening on UDS...")

	wakeBuf := make([]byte, 1024) // 大一点的 buffer，用来吸干积压的信号

	for {
		// 1. 外层循环：负责处理被测程序的连接与重连
		conn, err := e.listener.Accept()
		if err != nil {
			fmt.Printf("Accept error: %v\n", err)
			continue
		}
		fmt.Println("Tracee connected! Entering hot loop.")

		// 2. 内层循环：核心无锁收割逻辑
		e.hotHarvestLoop(conn, wakeBuf)

		fmt.Println("Tracee disconnected. Waiting for next connection...")
		conn.Close()
	}
}

// 提取出一个专门的收割函数，方便复用
func (e *TracerEngine) doScan() int {
	totalHarvested := 0
	allocated := atomic.LoadUint32(&e.header.AllocatedCount)
	if allocated > MaxStations {
		allocated = MaxStations
	}

	for i := uint32(0); i < allocated; i++ {
		totalHarvested += e.stations[i].Harvest(&e.lastSeen[i], e.writer)
	}
	return totalHarvested
}

// hotHarvestLoop 真正的无锁高性能核心
func (e *TracerEngine) hotHarvestLoop(conn net.Conn, wakeBuf []byte) {
	for {
		// 第一步：狂奔模式扫描
		harvested := e.doScan()

		if harvested > 0 {
			// 如果有数据，说明系统繁忙，继续狂奔，不让出 CPU
			continue
		}

		// 第二步：准备睡眠前的安全落盘
		e.writer.Flush()

		// 第三步：宣告即将睡眠 (Memory Barrier)
		atomic.StoreUint32(&e.header.TracerSleeping, 1)

		// 🔴 核心修复：Double-Check！
		// 在宣告睡眠后，必须再扫一次！
		// 因为在 "扫完" 到 "宣告睡眠" 的间隙，可能有探针写入了数据！
		if e.doScan() > 0 {
			// 糟糕！有探针在我闭眼的一瞬间写了数据！
			// 取消睡眠，撤回标志，继续干活
			atomic.StoreUint32(&e.header.TracerSleeping, 0)
			continue
		}

		// 第四步：真正进入零消耗睡眠
		// 此时如果 C++ 写入数据，一定会看到 TracerSleeping == 1，从而发信号
		n, err := conn.Read(wakeBuf)
		if err != nil || n == 0 {
			// UDS 连接断开（被测程序崩溃或正常退出）
			// 退出热循环，回到外层等待下一次 Accept
			atomic.StoreUint32(&e.header.TracerSleeping, 0)
			return
		}

		// 醒来后立刻撤销睡眠标志
		atomic.StoreUint32(&e.header.TracerSleeping, 0)
	}
}

// Close 优雅释放资源，供 main.go 的 defer 和信号监听调用
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
