package export

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ─── Fixtures ─────────────────────────────────────────────────────────────────

var sampleRecords = []TraceRecord{
	{ProbeID: 1, TID: 100, Addr: "0x00007fff00001234", Seq: 2, IsActive: false, TS: 1_000_000},
	{ProbeID: 1, TID: 100, Addr: "0x0000000000000000", Seq: 4, IsActive: true, TS: 2_000_000},
	{ProbeID: 2, TID: 200, Addr: "0x00007fff00005678", Seq: 2, IsActive: false, TS: 1_500_000},
	{ProbeID: 2, TID: 200, Addr: "0x0000000000000000", Seq: 4, IsActive: true, TS: 2_500_000},
	{ProbeID: 3, TID: 300, Addr: "0xdeadbeefcafebabe", Seq: 2, IsActive: false, TS: 3_000_000},
}

func writeTempJSONL(t *testing.T, records []TraceRecord) string {
	t.Helper()
	f, err := os.CreateTemp("", "export_test_*.jsonl")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	for _, r := range records {
		line, _ := json.Marshal(r)
		f.Write(line)
		f.WriteString("\n")
	}
	return f.Name()
}

func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ─── StreamJSONL ──────────────────────────────────────────────────────────────

func TestStreamJSONLReadsAllRecords(t *testing.T) {
	name := writeTempJSONL(t, sampleRecords)
	defer os.Remove(name)

	var got []TraceRecord
	if err := StreamJSONL(name, func(r TraceRecord) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatalf("StreamJSONL: %v", err)
	}
	if len(got) != len(sampleRecords) {
		t.Fatalf("record count = %d, want %d", len(got), len(sampleRecords))
	}
	for i, want := range sampleRecords {
		if got[i] != want {
			t.Errorf("record[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestStreamJSONLEmptyFile(t *testing.T) {
	name := writeTempJSONL(t, nil)
	defer os.Remove(name)

	var count int
	if err := StreamJSONL(name, func(TraceRecord) error { count++; return nil }); err != nil {
		t.Fatalf("StreamJSONL empty: %v", err)
	}
	if count != 0 {
		t.Errorf("empty file: count = %d, want 0", count)
	}
}

func TestStreamJSONLSkipsBlankLines(t *testing.T) {
	f, _ := os.CreateTemp("", "blank_*.jsonl")
	name := f.Name()
	defer os.Remove(name)

	r := sampleRecords[0]
	line, _ := json.Marshal(r)
	f.WriteString("\n\n")
	f.Write(line)
	f.WriteString("\n\n")
	f.Close()

	var count int
	if err := StreamJSONL(name, func(TraceRecord) error { count++; return nil }); err != nil {
		t.Fatalf("StreamJSONL blanks: %v", err)
	}
	if count != 1 {
		t.Errorf("blank-lines file: count = %d, want 1", count)
	}
}

func TestStreamJSONLMissingFile(t *testing.T) {
	err := StreamJSONL("/nonexistent_xyz/test.jsonl", func(TraceRecord) error { return nil })
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestStreamJSONLMalformedLine(t *testing.T) {
	f, _ := os.CreateTemp("", "bad_*.jsonl")
	name := f.Name()
	defer os.Remove(name)
	f.WriteString("{not valid json}\n")
	f.Close()

	err := StreamJSONL(name, func(TraceRecord) error { return nil })
	if err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}

func TestStreamJSONLLargeFile(t *testing.T) {
	const n = 1_000
	records := make([]TraceRecord, n)
	for i := range records {
		records[i] = TraceRecord{
			ProbeID:  uint64(i%10 + 1),
			TID:      uint64(100 + i%4),
			Addr:     "0x0000000000001234",
			Seq:      uint64(i*2 + 2),
			IsActive: i%2 == 0,
			TS:       uint64(i * 1_000_000),
		}
	}
	name := writeTempJSONL(t, records)
	defer os.Remove(name)

	var count int
	if err := StreamJSONL(name, func(TraceRecord) error { count++; return nil }); err != nil {
		t.Fatalf("StreamJSONL large: %v", err)
	}
	if count != n {
		t.Errorf("large file: count = %d, want %d", count, n)
	}
}

// ─── ExportJSONLToDataFrameCSV ────────────────────────────────────────────────

func TestExportDataFrameCSVBasic(t *testing.T) {
	name := writeTempJSONL(t, sampleRecords)
	defer os.Remove(name)
	csvPath := name + ".csv"
	defer os.Remove(csvPath)

	if err := ExportJSONLToDataFrameCSV(name, csvPath); err != nil {
		t.Fatalf("ExportJSONLToDataFrameCSV: %v", err)
	}

	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatalf("open csv: %v", err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}

	// header + data rows
	if len(rows) != len(sampleRecords)+1 {
		t.Fatalf("row count = %d, want %d", len(rows), len(sampleRecords)+1)
	}
}

func TestExportDataFrameCSVHeaderRow(t *testing.T) {
	name := writeTempJSONL(t, sampleRecords)
	defer os.Remove(name)
	csvPath := name + ".csv"
	defer os.Remove(csvPath)

	ExportJSONLToDataFrameCSV(name, csvPath)

	f, _ := os.Open(csvPath)
	defer f.Close()
	rows, _ := csv.NewReader(f).ReadAll()

	want := []string{"probe_id", "tid", "addr", "seq", "is_active", "ts"}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for i, col := range want {
		if i >= len(rows[0]) || rows[0][i] != col {
			t.Errorf("header[%d] = %q, want %q", i, rows[0][i], col)
		}
	}
}

func TestExportDataFrameCSVAddrPreserved(t *testing.T) {
	records := []TraceRecord{
		{ProbeID: 1, TID: 1, Addr: "0xcafebabe12345678", Seq: 2, IsActive: true, TS: 42},
	}
	name := writeTempJSONL(t, records)
	defer os.Remove(name)
	csvPath := name + ".csv"
	defer os.Remove(csvPath)

	ExportJSONLToDataFrameCSV(name, csvPath)

	f, _ := os.Open(csvPath)
	defer f.Close()
	rows, _ := csv.NewReader(f).ReadAll()
	if len(rows) < 2 {
		t.Fatal("expected 2 rows")
	}
	if rows[1][2] != "0xcafebabe12345678" {
		t.Errorf("addr = %q, want 0xcafebabe12345678", rows[1][2])
	}
}

func TestExportDataFrameCSVIsActiveBoolString(t *testing.T) {
	records := []TraceRecord{
		{Addr: "0x0000000000000000", Seq: 2, IsActive: true},
		{Addr: "0x0000000000000001", Seq: 4, IsActive: false},
	}
	name := writeTempJSONL(t, records)
	defer os.Remove(name)
	csvPath := name + ".csv"
	defer os.Remove(csvPath)

	ExportJSONLToDataFrameCSV(name, csvPath)

	f, _ := os.Open(csvPath)
	defer f.Close()
	rows, _ := csv.NewReader(f).ReadAll()
	if len(rows) < 3 {
		t.Fatal("expected 3 rows")
	}
	if rows[1][4] != "true" {
		t.Errorf("is_active row1 = %q, want true", rows[1][4])
	}
	if rows[2][4] != "false" {
		t.Errorf("is_active row2 = %q, want false", rows[2][4])
	}
}

func TestExportDataFrameCSVEmpty(t *testing.T) {
	name := writeTempJSONL(t, nil)
	defer os.Remove(name)
	csvPath := name + ".csv"
	defer os.Remove(csvPath)

	if err := ExportJSONLToDataFrameCSV(name, csvPath); err != nil {
		t.Fatalf("ExportJSONLToDataFrameCSV empty: %v", err)
	}

	f, _ := os.Open(csvPath)
	defer f.Close()
	rows, _ := csv.NewReader(f).ReadAll()
	if len(rows) != 1 {
		t.Errorf("empty input: rows = %d, want 1 (header only)", len(rows))
	}
}

func TestExportDataFrameCSVCreatesParentDir(t *testing.T) {
	name := writeTempJSONL(t, sampleRecords)
	defer os.Remove(name)

	dir, _ := os.MkdirTemp("", "csv_subdir_*")
	defer os.RemoveAll(dir)
	csvPath := dir + "/sub/trace.csv"

	if err := ExportJSONLToDataFrameCSV(name, csvPath); err != nil {
		t.Fatalf("ExportJSONLToDataFrameCSV subdir: %v", err)
	}
	if _, err := os.Stat(csvPath); os.IsNotExist(err) {
		t.Error("csv not created in new subdirectory")
	}
}

// ─── SQLite export ────────────────────────────────────────────────────────────

func TestSQLiteSchemaSQL(t *testing.T) {
	sql := sqliteSchemaSQL()
	for _, want := range []string{
		"CREATE TABLE",
		DefaultTableName,
		"probe_id",
		"tid",
		"addr",
		"seq",
		"is_active",
		"ts",
		"CREATE INDEX",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("schemaSQL missing %q", want)
		}
	}
}

func TestExportJSONLToSQLite(t *testing.T) {
	if !hasBinary("sqlite3") {
		t.Skip("sqlite3 not in PATH")
	}

	name := writeTempJSONL(t, sampleRecords)
	defer os.Remove(name)
	dbPath := name + ".sqlite"
	defer os.Remove(dbPath)

	if err := ExportJSONLToSQLite(name, dbPath); err != nil {
		t.Fatalf("ExportJSONLToSQLite: %v", err)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat sqlite db: %v", err)
	}
	if info.Size() == 0 {
		t.Error("sqlite db is empty")
	}
}

func TestExportJSONLToSQLiteEmpty(t *testing.T) {
	if !hasBinary("sqlite3") {
		t.Skip("sqlite3 not in PATH")
	}
	name := writeTempJSONL(t, nil)
	defer os.Remove(name)
	dbPath := name + ".sqlite"
	defer os.Remove(dbPath)

	if err := ExportJSONLToSQLite(name, dbPath); err != nil {
		t.Fatalf("ExportJSONLToSQLite empty: %v", err)
	}
}

func TestExportJSONLToSQLiteRowCount(t *testing.T) {
	if !hasBinary("sqlite3") {
		t.Skip("sqlite3 not in PATH")
	}

	name := writeTempJSONL(t, sampleRecords)
	defer os.Remove(name)
	dbPath := name + ".sqlite"
	defer os.Remove(dbPath)

	ExportJSONLToSQLite(name, dbPath)

	out, err := exec.Command("sqlite3", dbPath,
		"SELECT COUNT(*) FROM "+DefaultTableName+";").Output()
	if err != nil {
		t.Fatalf("sqlite3 count: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := "5" // len(sampleRecords)
	if got != want {
		t.Errorf("sqlite row count = %s, want %s", got, want)
	}
}

// ─── MySQL schema (no service needed) ────────────────────────────────────────

func TestMySQLSchemaSQL(t *testing.T) {
	sql := mysqlSchemaSQL("testdb", "test_events")
	for _, want := range []string{
		"CREATE DATABASE",
		"`testdb`",
		"CREATE TABLE",
		"`test_events`",
		"probe_id",
		"tid",
		"addr",
		"seq",
		"is_active",
		"ts",
		"PRIMARY KEY",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("mysqlSchemaSQL missing %q", want)
		}
	}
}

func TestMySQLSchemaDefaultNames(t *testing.T) {
	sql := mysqlSchemaSQL(DefaultDatabaseName, DefaultTableName)
	if !strings.Contains(sql, "`"+DefaultDatabaseName+"`") {
		t.Errorf("default database name not found")
	}
	if !strings.Contains(sql, "`"+DefaultTableName+"`") {
		t.Errorf("default table name not found")
	}
}

func TestExportMySQLSchemaScript(t *testing.T) {
	dir, _ := os.MkdirTemp("", "mysql_schema_*")
	defer os.RemoveAll(dir)

	out := dir + "/schema.sql"
	if err := ExportMySQLSchemaScript(out, "mydb"); err != nil {
		t.Fatalf("ExportMySQLSchemaScript: %v", err)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "CREATE TABLE") {
		t.Error("schema script missing CREATE TABLE")
	}
}

// ─── PostgreSQL schema (no service needed) ───────────────────────────────────

func TestPostgreSQLSchemaSQL(t *testing.T) {
	sql := postgreSQLSchemaSQL("pgdb", "pg_events")
	for _, want := range []string{
		"CREATE TABLE",
		`"pg_events"`,
		"probe_id",
		"tid",
		"addr",
		"seq",
		"is_active",
		"ts",
		"BIGSERIAL",
		"CREATE INDEX",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("postgreSQLSchemaSQL missing %q", want)
		}
	}
}

func TestExportPostgreSQLSchemaScript(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pg_schema_*")
	defer os.RemoveAll(dir)

	out := dir + "/pg_schema.sql"
	if err := ExportPostgreSQLSchemaScript(out, "pgdb"); err != nil {
		t.Fatalf("ExportPostgreSQLSchemaScript: %v", err)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "CREATE TABLE") {
		t.Error("schema script missing CREATE TABLE")
	}
}

// ─── String escape helpers ────────────────────────────────────────────────────

func TestEscapeSQLiteString(t *testing.T) {
	cases := []struct{ input, want string }{
		{"normal", "normal"},
		{"it's", "it''s"},
		{"a''b", "a''''b"},
		{"", ""},
	}
	for _, c := range cases {
		if got := escapeSQLiteString(c.input); got != c.want {
			t.Errorf("escapeSQLiteString(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestEscapeMySQLString(t *testing.T) {
	cases := []struct{ input, want string }{
		{"normal", "normal"},
		{"it's", "it''s"},
		{`a\b`, `a\\b`},
		{`a\'b`, `a\\''b`},
		{"", ""},
	}
	for _, c := range cases {
		if got := escapeMySQLString(c.input); got != c.want {
			t.Errorf("escapeMySQLString(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestEscapePostgresString(t *testing.T) {
	cases := []struct{ input, want string }{
		{"normal", "normal"},
		{"it's", "it''s"},
		{"a''b", "a''''b"},
	}
	for _, c := range cases {
		if got := escapePostgresString(c.input); got != c.want {
			t.Errorf("escapePostgresString(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ─── Identifier quoting ───────────────────────────────────────────────────────

func TestQuoteMySQLIdentifier(t *testing.T) {
	cases := []struct{ input, want string }{
		{"table", "`table`"},
		{"my`table", "`my``table`"},
		{"", "``"},
	}
	for _, c := range cases {
		if got := quoteMySQLIdentifier(c.input); got != c.want {
			t.Errorf("quoteMySQLIdentifier(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestQuotePostgresIdentifier(t *testing.T) {
	cases := []struct{ input, want string }{
		{"table", `"table"`},
		{`my"table`, `"my""table"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := quotePostgresIdentifier(c.input); got != c.want {
			t.Errorf("quotePostgresIdentifier(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// ─── Utility helpers ──────────────────────────────────────────────────────────

func TestDefaultString(t *testing.T) {
	if got := defaultString("", "fallback"); got != "fallback" {
		t.Errorf("defaultString empty = %q, want fallback", got)
	}
	if got := defaultString("  ", "fallback"); got != "fallback" {
		t.Errorf("defaultString spaces = %q, want fallback", got)
	}
	if got := defaultString("value", "fallback"); got != "value" {
		t.Errorf("defaultString value = %q, want value", got)
	}
}

func TestBoolToInt(t *testing.T) {
	if got := boolToInt(true); got != 1 {
		t.Errorf("boolToInt(true) = %d, want 1", got)
	}
	if got := boolToInt(false); got != 0 {
		t.Errorf("boolToInt(false) = %d, want 0", got)
	}
}

func TestEnsureParentDir(t *testing.T) {
	dir, _ := os.MkdirTemp("", "parentdir_*")
	defer os.RemoveAll(dir)

	nested := dir + "/a/b/c/file.txt"
	if err := ensureParentDir(nested); err != nil {
		t.Fatalf("ensureParentDir: %v", err)
	}
	if _, err := os.Stat(dir + "/a/b/c"); os.IsNotExist(err) {
		t.Error("parent dirs not created")
	}
}

func TestEnsureParentDirCurrentDir(t *testing.T) {
	// path with no parent (just "file.txt") should be a no-op
	if err := ensureParentDir("file.txt"); err != nil {
		t.Errorf("ensureParentDir current dir: %v", err)
	}
}

// ─── Constants ────────────────────────────────────────────────────────────────

func TestDefaultNames(t *testing.T) {
	if DefaultDatabaseName == "" {
		t.Error("DefaultDatabaseName is empty")
	}
	if DefaultTableName == "" {
		t.Error("DefaultTableName is empty")
	}
}
