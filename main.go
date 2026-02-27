package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"coroTracer/deepdive"
	"coroTracer/engine" //
	"coroTracer/export"
)

func main() {
	// 1. 定义命令行参数
	n := flag.Uint("n", 128, "Number of stations (coroutines) to allocate")
	cmdStr := flag.String("cmd", "", "Target command to execute and trace (e.g., './my_cpp_coro')")
	shmPath := flag.String("shm", "/tmp/corotracer.shm", "Path to shared memory file")
	sockPath := flag.String("sock", "/tmp/corotracer.sock", "Path to Unix Domain Socket")
	logPath := flag.String("out", "trace_output.jsonl", "Output JSONL file path")
	deepDiveMode := flag.Bool("deepdive", false, "Run offline analysis on an existing JSONL trace file")
	htmlExportMode := flag.Bool("html", false, "Export trace to interactive HTML dashboard")
	flag.Parse()

	// 🔀 分支逻辑：进入深潜分析模式
	if *deepDiveMode {
		inPath := *logPath // 复用 -out 参数作为输入文件
		outMd := "coro_report.md"

		fmt.Printf("🚀 Starting DeepDive Analysis on %s...\n", inPath)
		// 调用 deepdive 包里的函数
		if err := deepdive.RunDeepDive(inPath, outMd); err != nil {
			log.Fatalf("DeepDive failed: %v", err)
		}
		os.Exit(0)
	}

	if *htmlExportMode {
		inPath := *logPath
		outHtml := "coro_dashboard.html"
		if err := export.GenerateHTML(inPath, outHtml); err != nil {
			log.Fatalf("HTML Export failed: %v", err)
		}
		os.Exit(0)
	}

	if *cmdStr == "" {
		log.Fatal("Error: -cmd parameter is required. Example: ./coroTracer -n 100 -cmd './redis-test'")
	}

	fmt.Printf("🚀 coroTracer Launcher Started\n")
	fmt.Printf("📦 Allocating %d Stations (Memory: %d Bytes)\n", *n, 64+(*n*1024))

	// 2. 初始化收割机引擎
	tracer, err := engine.NewTracerEngine(uint32(*n), *shmPath, *sockPath, *logPath)
	if err != nil {
		log.Fatalf("Failed to initialize Tracer Engine: %v", err)
	}
	defer tracer.Close()

	// 3. 在后台 Goroutine 启动收割事件循环
	go func() {
		if err := tracer.Run(); err != nil {
			log.Printf("Tracer engine exited: %v\n", err)
		}
	}()

	// 4. 准备目标命令 (Tracee)
	// 使用 sh -c 可以支持带参数的命令，比如 -cmd "./my_prog --threads 4"
	cmd := exec.Command("sh", "-c", *cmdStr)

	// 🔴 核心：通过环境变量将 cTP 协议的连接信息注入给子进程
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CTP_SHM_PATH=%s", *shmPath),
		fmt.Sprintf("CTP_SOCK_PATH=%s", *sockPath),
		// 我们甚至可以把 n 传过去，让被测程序知道自己的并发上限
		fmt.Sprintf("CTP_MAX_STATIONS=%d", *n),
	)

	// 将子进程的输出重定向到主控台，方便调试
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 5. 监听系统的中断信号 (Ctrl+C)，优雅退出
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n🛑 Received interrupt signal, shutting down...")
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM) // 顺手把子进程也杀掉
		}
		tracer.Close()
		os.Exit(0)
	}()

	// 6. 正式拉起被测子进程
	fmt.Printf("🏃 Executing target: %s\n", *cmdStr)
	if err := cmd.Run(); err != nil {
		log.Fatalf("Target command exited with error: %v", err)
	}

	fmt.Println("✅ Target command finished successfully. coroTracer exiting.")
}
