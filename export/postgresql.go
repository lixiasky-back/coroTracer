package export

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type PostgreSQLExportOptions struct {
	Command       string
	Host          string
	Port          int
	User          string
	Password      string
	Database      string
	Table         string
	MaintenanceDB string
	SSLMode       string
}

// ExportJSONLToPostgreSQL converts a trace JSONL file directly into a
// PostgreSQL table by streaming SQL through the local psql CLI.
func ExportJSONLToPostgreSQL(jsonlPath string, options PostgreSQLExportOptions) error {
	command := defaultString(options.Command, "psql")
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("psql binary %q not found in PATH: %w", command, err)
	}

	databaseName := defaultString(options.Database, DefaultDatabaseName)
	tableName := defaultString(options.Table, DefaultTableName)

	if err := ensurePostgreSQLDatabaseExists(command, options, databaseName); err != nil {
		return err
	}

	args := postgreSQLCLIArgs(options, databaseName)
	cmd := exec.Command(command, args...)
	cmd.Env = postgreSQLEnv(options)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open psql stdin: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start psql CLI: %w", err)
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
	if _, err := fmt.Fprintln(writer, "SET client_encoding = 'UTF8';"); err != nil {
		return abort(fmt.Errorf("write postgres encoding setup: %w", err))
	}
	if _, err := fmt.Fprintln(writer, "SET standard_conforming_strings = on;"); err != nil {
		return abort(fmt.Errorf("write postgres string setup: %w", err))
	}
	if _, err := fmt.Fprint(writer, postgreSQLSchemaSQL(databaseName, tableName)); err != nil {
		return abort(fmt.Errorf("write postgres schema setup: %w", err))
	}
	if _, err := fmt.Fprintln(writer, "BEGIN;"); err != nil {
		return abort(fmt.Errorf("open postgres transaction: %w", err))
	}

	insertSQL := "INSERT INTO public." + quotePostgresIdentifier(tableName) + " (probe_id, tid, addr, seq, is_active, ts) VALUES (%d, %d, '%s', %d, %t, %d);\n"
	if err := StreamJSONL(jsonlPath, func(record TraceRecord) error {
		_, err := fmt.Fprintf(
			writer,
			insertSQL,
			record.ProbeID,
			record.TID,
			escapePostgresString(record.Addr),
			record.Seq,
			record.IsActive,
			record.TS,
		)
		return err
	}); err != nil {
		return abort(fmt.Errorf("stream jsonl into postgres inserts: %w", err))
	}

	if _, err := fmt.Fprintln(writer, "COMMIT;"); err != nil {
		return abort(fmt.Errorf("commit postgres transaction: %w", err))
	}

	if err := writer.Flush(); err != nil {
		return abort(fmt.Errorf("flush postgres input stream: %w", err))
	}
	if err := stdin.Close(); err != nil {
		return abort(fmt.Errorf("close postgres input stream: %w", err))
	}

	if err := cmd.Wait(); err != nil {
		psqlErr := strings.TrimSpace(stderr.String())
		if psqlErr == "" {
			return fmt.Errorf("psql CLI execution failed: %w", err)
		}
		return fmt.Errorf("psql CLI execution failed: %w: %s", err, psqlErr)
	}

	return nil
}

// ExportPostgreSQLSchemaScript writes a PostgreSQL schema bootstrap script.
func ExportPostgreSQLSchemaScript(outputPath, databaseName string) error {
	if err := ensureParentDir(outputPath); err != nil {
		return fmt.Errorf("create parent directory for postgres schema output: %w", err)
	}

	databaseName = defaultString(databaseName, DefaultDatabaseName)
	script := postgreSQLSchemaSQL(databaseName, DefaultTableName)

	if err := os.WriteFile(outputPath, []byte(script), 0o644); err != nil {
		return fmt.Errorf("write postgres schema script %q: %w", outputPath, err)
	}

	return nil
}

func ensurePostgreSQLDatabaseExists(command string, options PostgreSQLExportOptions, databaseName string) error {
	maintenanceDB := defaultString(options.MaintenanceDB, "postgres")
	checkArgs := append(postgreSQLCLIArgs(options, maintenanceDB), "-tAc",
		"SELECT 1 FROM pg_database WHERE datname = '"+escapePostgresString(databaseName)+"';",
	)

	checkCmd := exec.Command(command, checkArgs...)
	checkCmd.Env = postgreSQLEnv(options)

	var checkStderr bytes.Buffer
	checkCmd.Stderr = &checkStderr

	output, err := checkCmd.Output()
	if err != nil {
		psqlErr := strings.TrimSpace(checkStderr.String())
		if psqlErr == "" {
			return fmt.Errorf("check postgres database existence: %w", err)
		}
		return fmt.Errorf("check postgres database existence: %w: %s", err, psqlErr)
	}

	if strings.TrimSpace(string(output)) == "1" {
		return nil
	}

	createArgs := append(postgreSQLCLIArgs(options, maintenanceDB), "-c",
		"CREATE DATABASE "+quotePostgresIdentifier(databaseName)+";",
	)
	createCmd := exec.Command(command, createArgs...)
	createCmd.Env = postgreSQLEnv(options)

	var createStderr bytes.Buffer
	createCmd.Stderr = &createStderr

	if err := createCmd.Run(); err != nil {
		psqlErr := strings.TrimSpace(createStderr.String())
		if psqlErr == "" {
			return fmt.Errorf("create postgres database %q: %w", databaseName, err)
		}
		return fmt.Errorf("create postgres database %q: %w: %s", databaseName, err, psqlErr)
	}

	return nil
}

func postgreSQLCLIArgs(options PostgreSQLExportOptions, databaseName string) []string {
	host := defaultString(options.Host, "127.0.0.1")
	port := options.Port
	if port == 0 {
		port = 5432
	}

	args := []string{
		"-v", "ON_ERROR_STOP=1",
		"--host", host,
		"--port", fmt.Sprintf("%d", port),
		"--dbname", databaseName,
	}

	if options.User != "" {
		args = append(args, "--username", options.User)
	}

	return args
}

func postgreSQLEnv(options PostgreSQLExportOptions) []string {
	env := os.Environ()
	if options.Password != "" {
		env = append(env, "PGPASSWORD="+options.Password)
	}
	if options.SSLMode != "" {
		env = append(env, "PGSSLMODE="+options.SSLMode)
	}
	return env
}

func postgreSQLSchemaSQL(databaseName, tableName string) string {
	db := quotePostgresIdentifier(databaseName)
	table := quotePostgresIdentifier(defaultString(tableName, DefaultTableName))

	return fmt.Sprintf(`-- Optional database creation step:
-- CREATE DATABASE %s;
-- Reconnect to the target database before running the statements below.

CREATE TABLE IF NOT EXISTS public.%s (
  id BIGSERIAL PRIMARY KEY,
  probe_id NUMERIC(20,0) NOT NULL,
  tid NUMERIC(20,0) NOT NULL,
  addr VARCHAR(18) NOT NULL,
  seq NUMERIC(20,0) NOT NULL,
  is_active BOOLEAN NOT NULL,
  ts NUMERIC(20,0) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_coro_trace_events_probe_seq
  ON public.%s (probe_id, seq);
CREATE INDEX IF NOT EXISTS idx_coro_trace_events_tid_ts
  ON public.%s (tid, ts);
CREATE INDEX IF NOT EXISTS idx_coro_trace_events_ts
  ON public.%s (ts);

-- Example CSV load. This matches ExportJSONLToDataFrameCSV output.
-- \copy public.%s (probe_id, tid, addr, seq, is_active, ts)
--   FROM '/path/to/trace.csv'
--   WITH (FORMAT csv, HEADER true);
`, db, table, table, table, table, table)
}
