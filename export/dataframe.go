package export

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
)

// ExportJSONLToDataFrameCSV converts the trace JSONL into CSV, which is a
// zero-dependency DataFrame-friendly format for pandas, polars, DuckDB, and R.
func ExportJSONLToDataFrameCSV(jsonlPath, csvPath string) error {
	if err := ensureParentDir(csvPath); err != nil {
		return fmt.Errorf("create parent directory for csv output: %w", err)
	}

	file, err := os.Create(csvPath)
	if err != nil {
		return fmt.Errorf("create csv output %q: %w", csvPath, err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)

	if err := writer.Write([]string{"probe_id", "tid", "addr", "seq", "is_active", "ts"}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	if err := StreamJSONL(jsonlPath, func(record TraceRecord) error {
		return writer.Write([]string{
			strconv.FormatUint(record.ProbeID, 10),
			strconv.FormatUint(record.TID, 10),
			record.Addr,
			strconv.FormatUint(record.Seq, 10),
			strconv.FormatBool(record.IsActive),
			strconv.FormatUint(record.TS, 10),
		})
	}); err != nil {
		return err
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("flush csv output %q: %w", csvPath, err)
	}

	return nil
}
