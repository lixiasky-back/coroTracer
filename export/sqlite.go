package export

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ExportJSONLToSQLite converts a trace JSONL file into a SQLite database file.
//
// Runtime note: this exporter uses the local sqlite3 CLI so the project keeps
// its Go dependency set minimal.
func ExportJSONLToSQLite(jsonlPath, sqlitePath string) error {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return fmt.Errorf("sqlite3 binary not found in PATH: %w", err)
	}

	if err := ensureParentDir(sqlitePath); err != nil {
		return fmt.Errorf("create parent directory for sqlite output: %w", err)
	}

	if err := os.Remove(sqlitePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing sqlite database %q: %w", sqlitePath, err)
	}

	cmd := exec.Command("sqlite3", sqlitePath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open sqlite3 stdin: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sqlite3: %w", err)
	}

	abort := func(writeErr error) error {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return writeErr
	}

	writer := bufio.NewWriter(stdin)

	if _, err := fmt.Fprintln(writer, "PRAGMA journal_mode=WAL;"); err != nil {
		return abort(fmt.Errorf("write sqlite pragma: %w", err))
	}
	if _, err := fmt.Fprintln(writer, "PRAGMA synchronous=NORMAL;"); err != nil {
		return abort(fmt.Errorf("write sqlite pragma: %w", err))
	}
	if _, err := fmt.Fprint(writer, sqliteSchemaSQL()); err != nil {
		return abort(fmt.Errorf("write sqlite schema: %w", err))
	}
	if _, err := fmt.Fprintln(writer, "BEGIN IMMEDIATE;"); err != nil {
		return abort(fmt.Errorf("open sqlite transaction: %w", err))
	}

	insertSQL := "INSERT INTO " + DefaultTableName + " (probe_id, tid, addr, seq, is_active, ts) VALUES ('%d', %d, '%s', %d, %d, %d);\n"
	if err := StreamJSONL(jsonlPath, func(record TraceRecord) error {
		_, err := fmt.Fprintf(
			writer,
			insertSQL,
			record.ProbeID,
			record.TID,
			escapeSQLiteString(record.Addr),
			record.Seq,
			boolToInt(record.IsActive),
			record.TS,
		)
		return err
	}); err != nil {
		return abort(fmt.Errorf("stream jsonl into sqlite inserts: %w", err))
	}

	if _, err := fmt.Fprintln(writer, "COMMIT;"); err != nil {
		return abort(fmt.Errorf("commit sqlite transaction: %w", err))
	}

	if err := writer.Flush(); err != nil {
		return abort(fmt.Errorf("flush sqlite stream: %w", err))
	}
	if err := stdin.Close(); err != nil {
		return abort(fmt.Errorf("close sqlite stream: %w", err))
	}

	if err := cmd.Wait(); err != nil {
		sqliteErr := strings.TrimSpace(stderr.String())
		if sqliteErr == "" {
			return fmt.Errorf("sqlite3 execution failed: %w", err)
		}
		return fmt.Errorf("sqlite3 execution failed: %w: %s", err, sqliteErr)
	}

	return nil
}

func sqliteSchemaSQL() string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  probe_id TEXT NOT NULL,
  tid INTEGER NOT NULL,
  addr TEXT NOT NULL,
  seq INTEGER NOT NULL,
  is_active INTEGER NOT NULL CHECK (is_active IN (0, 1)),
  ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_%s_probe_seq ON %s (probe_id, seq);
CREATE INDEX IF NOT EXISTS idx_%s_tid_ts ON %s (tid, ts);
CREATE INDEX IF NOT EXISTS idx_%s_ts ON %s (ts);
`, DefaultTableName, DefaultTableName, DefaultTableName, DefaultTableName, DefaultTableName, DefaultTableName, DefaultTableName)
}
