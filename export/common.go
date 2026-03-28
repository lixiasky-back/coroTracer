package export

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	DefaultDatabaseName = "coro_tracer"
	DefaultTableName    = "coro_trace_events"
)

type TraceRecord struct {
	ProbeID  uint64 `json:"probe_id"`
	TID      uint64 `json:"tid"`
	Addr     string `json:"addr"`
	Seq      uint64 `json:"seq"`
	IsActive bool   `json:"is_active"`
	TS       uint64 `json:"ts"`
}

// StreamJSONL walks the trace JSONL file line by line so large traces can be
// exported without loading the whole file into memory.
func StreamJSONL(jsonlPath string, fn func(record TraceRecord) error) error {
	file, err := os.Open(jsonlPath)
	if err != nil {
		return fmt.Errorf("open jsonl %q: %w", jsonlPath, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var record TraceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return fmt.Errorf("decode jsonl line %d: %w", lineNo, err)
		}

		if err := fn(record); err != nil {
			return fmt.Errorf("process jsonl line %d: %w", lineNo, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan jsonl %q: %w", jsonlPath, err)
	}

	return nil
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func escapeSQLiteString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func escapeMySQLString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, "'", "''")
}

func escapePostgresString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func quoteMySQLIdentifier(ident string) string {
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}

func quotePostgresIdentifier(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
