package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/lixiasky-back/coroTracer/engine"
)

func main() {
	// 1. Define command-line arguments
	n := flag.Uint("n", 128, "Number of stations (coroutines) to allocate")
	cmdStr := flag.String("cmd", "", "Target command to execute and trace (e.g., './my_cpp_coro')")
	shmPath := flag.String("shm", "/tmp/corotracer.shm", "Path to shared memory file")
	sockPath := flag.String("sock", "/tmp/corotracer.sock", "Path to Unix Domain Socket")
	logPath := flag.String("out", "trace_output.jsonl", "Output JSONL file path")
	flag.Parse()

	if *cmdStr == "" {
		log.Fatal("Error: -cmd parameter is required. Example: ./coroTracer -n 100 -cmd './redis-test'")
	}

	fmt.Printf("🚀 coroTracer Launcher Started\n")
	fmt.Printf("📦 Allocating %d Stations (Memory: %d Bytes)\n", *n, 64+(*n*1024))

	// 2. Initialize the harvester engine
	tracer, err := engine.NewTracerEngine(uint32(*n), *shmPath, *sockPath, *logPath)
	if err != nil {
		log.Fatalf("Failed to initialize Tracer Engine: %v", err)
	}
	defer tracer.Close()

	// 3. Start the harvesting event loop in a background Goroutine
	go func() {
		if err := tracer.Run(); err != nil {
			log.Printf("Tracer engine exited: %v\n", err)
		}
	}()

	// 4. Prepare the target command (Tracee)
	// Using sh -c enables support for commands with arguments, e.g., -cmd "./my_prog --threads 4"
	cmd := exec.Command("sh", "-c", *cmdStr)

	// 🔴 Core: Inject connection information of the cTP protocol into the child process via environment variables
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("CTP_SHM_PATH=%s", *shmPath),
		fmt.Sprintf("CTP_SOCK_PATH=%s", *sockPath),
		// We can even pass the value of n to let the tested program know its concurrency limit
		fmt.Sprintf("CTP_MAX_STATIONS=%d", *n),
	)

	// Redirect the output of the child process to the main console for easy debugging
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 5. Listen for system interrupt signals (Ctrl+C) for graceful exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n🛑 Received interrupt signal, shutting down...")
		if cmd.Process != nil {
			cmd.Process.Signal(syscall.SIGTERM)
		}
		tracer.Close()
		os.Exit(0)
	}()

	// 6. Officially launch the tested child process
	fmt.Printf("🏃 Executing target: %s\n", *cmdStr)
	if err := cmd.Run(); err != nil {
		log.Fatalf("Target command exited with error: %v", err)
	}

	fmt.Println("✅ Target command finished successfully. coroTracer exiting.")
}
