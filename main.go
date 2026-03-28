package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lixiasky-back/coroTracer/engine"
	exporter "github.com/lixiasky-back/coroTracer/export"
)

func main() {
	// 1. Define command-line arguments
	n := flag.Uint("n", 128, "Number of stations (coroutines) to allocate")
	cmdStr := flag.String("cmd", "", "Target command to execute and trace (e.g., './my_cpp_coro')")
	shmPath := flag.String("shm", "/tmp/corotracer.shm", "Path to shared memory file")
	sockPath := flag.String("sock", "/tmp/corotracer.sock", "Path to Unix Domain Socket")
	logPath := flag.String("out", "trace_output.jsonl", "Output JSONL file path")
	exportKind := flag.String("export", "", "Optional export target: sqlite | mysql | postgres | postgresql | dataframe | csv")
	inputPath := flag.String("in", "", "Input JSONL file for export-only mode. Defaults to -out.")
	sqlitePath := flag.String("sqlite-out", "", "Output SQLite database path. Defaults to <input>.sqlite")
	csvPath := flag.String("csv-out", "", "Output DataFrame-friendly CSV path. Defaults to <input>.csv")
	dbCLI := flag.String("db-cli", "", "Optional database CLI override. mysql export defaults to mysql; postgres export defaults to psql")
	dbHost := flag.String("db-host", "127.0.0.1", "Database host for mysql/postgres export")
	dbPort := flag.Int("db-port", 0, "Database port for mysql/postgres export. Defaults to 3306 for mysql and 5432 for postgres")
	dbUser := flag.String("db-user", "", "Database user for mysql/postgres export")
	dbPassword := flag.String("db-password", "", "Database password for mysql/postgres export")
	dbName := flag.String("db-name", exporter.DefaultDatabaseName, "Database name for mysql/postgres export")
	dbTable := flag.String("db-table", exporter.DefaultTableName, "Table name for mysql/postgres export")
	mysqlSocket := flag.String("mysql-socket", "", "MySQL Unix socket path. If set, host/port are ignored")
	pgMaintenanceDB := flag.String("pg-maintenance-db", "postgres", "PostgreSQL maintenance database used when auto-creating the target database")
	pgSSLMode := flag.String("pg-sslmode", "", "Optional PostgreSQL SSL mode passed via PGSSLMODE")
	flag.Parse()

	traceMode := strings.TrimSpace(*cmdStr) != ""
	exportMode := strings.TrimSpace(*exportKind) != ""

	if !traceMode && !exportMode {
		log.Fatal("Error: either -cmd or -export is required. Example: ./coroTracer -cmd './redis-test' or ./coroTracer -export sqlite -in trace_output.jsonl")
	}

	if traceMode && exportMode {
		log.Fatal("Error: -cmd and -export cannot be used together. Use -cmd only to collect JSONL, or use -export only to convert an existing JSONL file.")
	}

	if exportMode {
		exportInput := resolveExportInput(*inputPath, *logPath)
		if err := runExport(strings.TrimSpace(*exportKind), exportInput, exportConfig{
			sqlitePath:      *sqlitePath,
			csvPath:         *csvPath,
			dbCLI:           *dbCLI,
			dbHost:          *dbHost,
			dbPort:          *dbPort,
			dbUser:          *dbUser,
			dbPassword:      *dbPassword,
			dbName:          *dbName,
			dbTable:         *dbTable,
			mysqlSocket:     *mysqlSocket,
			pgMaintenanceDB: *pgMaintenanceDB,
			pgSSLMode:       *pgSSLMode,
		}); err != nil {
			log.Fatalf("Export failed: %v", err)
		}
		fmt.Println("✅ Export finished successfully.")
		return
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

type exportConfig struct {
	sqlitePath      string
	csvPath         string
	dbCLI           string
	dbHost          string
	dbPort          int
	dbUser          string
	dbPassword      string
	dbName          string
	dbTable         string
	mysqlSocket     string
	pgMaintenanceDB string
	pgSSLMode       string
}

func runExport(kind, inputPath string, cfg exportConfig) error {
	exportType := strings.ToLower(strings.TrimSpace(kind))

	switch exportType {
	case "sqlite":
		output := cfg.sqlitePath
		if strings.TrimSpace(output) == "" {
			output = deriveOutputPath(inputPath, ".sqlite")
		}
		fmt.Printf("📤 Exporting %s -> SQLite %s\n", inputPath, output)
		return exporter.ExportJSONLToSQLite(inputPath, output)
	case "dataframe", "csv":
		output := cfg.csvPath
		if strings.TrimSpace(output) == "" {
			output = deriveOutputPath(inputPath, ".csv")
		}
		fmt.Printf("📤 Exporting %s -> CSV %s\n", inputPath, output)
		return exporter.ExportJSONLToDataFrameCSV(inputPath, output)
	case "mysql":
		fmt.Printf("📤 Exporting %s -> MySQL %s.%s\n", inputPath, cfg.dbName, cfg.dbTable)
		return exporter.ExportJSONLToMySQL(inputPath, exporter.MySQLExportOptions{
			Command:  cfg.dbCLI,
			Host:     cfg.dbHost,
			Port:     cfg.dbPort,
			User:     cfg.dbUser,
			Password: cfg.dbPassword,
			Socket:   cfg.mysqlSocket,
			Database: cfg.dbName,
			Table:    cfg.dbTable,
		})
	case "postgres", "postgresql":
		fmt.Printf("📤 Exporting %s -> PostgreSQL %s.%s\n", inputPath, cfg.dbName, cfg.dbTable)
		return exporter.ExportJSONLToPostgreSQL(inputPath, exporter.PostgreSQLExportOptions{
			Command:       cfg.dbCLI,
			Host:          cfg.dbHost,
			Port:          cfg.dbPort,
			User:          cfg.dbUser,
			Password:      cfg.dbPassword,
			Database:      cfg.dbName,
			Table:         cfg.dbTable,
			MaintenanceDB: cfg.pgMaintenanceDB,
			SSLMode:       cfg.pgSSLMode,
		})
	default:
		return fmt.Errorf("unsupported export target %q", kind)
	}
}

func resolveExportInput(inputPath, defaultLogPath string) string {
	if strings.TrimSpace(inputPath) != "" {
		return inputPath
	}
	return defaultLogPath
}

func deriveOutputPath(inputPath, ext string) string {
	base := strings.TrimSuffix(inputPath, filepath.Ext(inputPath))
	if strings.TrimSpace(base) == "" || base == "." {
		return "trace_output" + ext
	}
	return base + ext
}
