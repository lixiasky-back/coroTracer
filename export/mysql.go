package export

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type MySQLExportOptions struct {
	Command  string
	Host     string
	Port     int
	User     string
	Password string
	Socket   string
	Database string
	Table    string
}

// ExportJSONLToMySQL converts a trace JSONL file directly into a MySQL table by
// streaming SQL through the local mysql CLI.
func ExportJSONLToMySQL(jsonlPath string, options MySQLExportOptions) error {
	command := defaultString(options.Command, "mysql")
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("mysql binary %q not found in PATH: %w", command, err)
	}

	databaseName := defaultString(options.Database, DefaultDatabaseName)
	tableName := defaultString(options.Table, DefaultTableName)

	args := []string{"--batch", "--raw"}
	if options.User != "" {
		args = append(args, "--user="+options.User)
	}
	if options.Socket != "" {
		args = append(args, "--socket="+options.Socket)
	} else {
		host := defaultString(options.Host, "127.0.0.1")
		port := options.Port
		if port == 0 {
			port = 3306
		}
		args = append(args, "--host="+host, fmt.Sprintf("--port=%d", port), "--protocol=tcp")
	}

	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	if options.Password != "" {
		cmd.Env = append(cmd.Env, "MYSQL_PWD="+options.Password)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open mysql stdin: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mysql CLI: %w", err)
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

	if _, err := fmt.Fprintln(writer, "SET NAMES utf8mb4;"); err != nil {
		return abort(fmt.Errorf("write mysql charset setup: %w", err))
	}
	if _, err := fmt.Fprint(writer, mysqlSchemaSQL(databaseName, tableName)); err != nil {
		return abort(fmt.Errorf("write mysql schema setup: %w", err))
	}
	if _, err := fmt.Fprintln(writer, "START TRANSACTION;"); err != nil {
		return abort(fmt.Errorf("open mysql transaction: %w", err))
	}

	insertSQL := "INSERT INTO " + quoteMySQLIdentifier(tableName) + " (probe_id, tid, addr, seq, is_active, ts) VALUES (%d, %d, '%s', %d, %t, %d);\n"
	if err := StreamJSONL(jsonlPath, func(record TraceRecord) error {
		_, err := fmt.Fprintf(
			writer,
			insertSQL,
			record.ProbeID,
			record.TID,
			escapeMySQLString(record.Addr),
			record.Seq,
			record.IsActive,
			record.TS,
		)
		return err
	}); err != nil {
		return abort(fmt.Errorf("stream jsonl into mysql inserts: %w", err))
	}

	if _, err := fmt.Fprintln(writer, "COMMIT;"); err != nil {
		return abort(fmt.Errorf("commit mysql transaction: %w", err))
	}

	if err := writer.Flush(); err != nil {
		return abort(fmt.Errorf("flush mysql input stream: %w", err))
	}
	if err := stdin.Close(); err != nil {
		return abort(fmt.Errorf("close mysql input stream: %w", err))
	}

	if err := cmd.Wait(); err != nil {
		mysqlErr := strings.TrimSpace(stderr.String())
		if mysqlErr == "" {
			return fmt.Errorf("mysql CLI execution failed: %w", err)
		}
		return fmt.Errorf("mysql CLI execution failed: %w: %s", err, mysqlErr)
	}

	return nil
}

// ExportMySQLSchemaScript writes a MySQL database bootstrap script.
func ExportMySQLSchemaScript(outputPath, databaseName string) error {
	if err := ensureParentDir(outputPath); err != nil {
		return fmt.Errorf("create parent directory for mysql schema output: %w", err)
	}

	databaseName = defaultString(databaseName, DefaultDatabaseName)
	script := mysqlSchemaSQL(databaseName, DefaultTableName)

	if err := os.WriteFile(outputPath, []byte(script), 0o644); err != nil {
		return fmt.Errorf("write mysql schema script %q: %w", outputPath, err)
	}

	return nil
}

func mysqlSchemaSQL(databaseName, tableName string) string {
	db := quoteMySQLIdentifier(databaseName)
	table := quoteMySQLIdentifier(defaultString(tableName, DefaultTableName))

	return fmt.Sprintf(`CREATE DATABASE IF NOT EXISTS %s
  CHARACTER SET utf8mb4
  COLLATE utf8mb4_unicode_ci;

USE %s;

CREATE TABLE IF NOT EXISTS %s (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  probe_id DECIMAL(20,0) NOT NULL,
  tid DECIMAL(20,0) NOT NULL,
  addr VARCHAR(18) NOT NULL,
  seq DECIMAL(20,0) NOT NULL,
  is_active BOOLEAN NOT NULL,
  ts DECIMAL(20,0) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_probe_seq (probe_id, seq),
  KEY idx_tid_ts (tid, ts),
  KEY idx_ts (ts)
) ENGINE=InnoDB
  DEFAULT CHARSET=utf8mb4
  COLLATE=utf8mb4_unicode_ci;

-- Example CSV load. This matches ExportJSONLToDataFrameCSV output.
-- LOAD DATA LOCAL INFILE '/path/to/trace.csv'
-- INTO TABLE %s
-- FIELDS TERMINATED BY ','
-- OPTIONALLY ENCLOSED BY '"'
-- IGNORE 1 LINES
-- (probe_id, tid, addr, seq, is_active, ts);
`, db, db, table, table)
}
