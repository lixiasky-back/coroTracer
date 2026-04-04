package main

import "testing"

// ─── deriveOutputPath ─────────────────────────────────────────────────────────

func TestDeriveOutputPath(t *testing.T) {
	cases := []struct {
		inputPath string
		ext       string
		want      string
	}{
		{"trace_output.jsonl", ".sqlite", "trace_output.sqlite"},
		{"trace_output.jsonl", ".csv", "trace_output.csv"},
		{"/tmp/run.jsonl", ".sqlite", "/tmp/run.sqlite"},
		{"noext", ".csv", "noext.csv"},
		{"a.b.c.jsonl", ".sqlite", "a.b.c.sqlite"},
		// empty / dot cases fall back to "trace_output<ext>"
		{"", ".csv", "trace_output.csv"},
		{".", ".csv", "trace_output.csv"},
	}
	for _, c := range cases {
		got := deriveOutputPath(c.inputPath, c.ext)
		if got != c.want {
			t.Errorf("deriveOutputPath(%q, %q) = %q, want %q",
				c.inputPath, c.ext, got, c.want)
		}
	}
}

// ─── resolveExportInput ───────────────────────────────────────────────────────

func TestResolveExportInput(t *testing.T) {
	cases := []struct {
		inputPath      string
		defaultLogPath string
		want           string
	}{
		{"explicit.jsonl", "default.jsonl", "explicit.jsonl"},
		{"", "default.jsonl", "default.jsonl"},
		{"  ", "default.jsonl", "default.jsonl"}, // whitespace-only → falls back to default
		{"path/to/file.jsonl", "fallback.jsonl", "path/to/file.jsonl"},
	}
	for _, c := range cases {
		got := resolveExportInput(c.inputPath, c.defaultLogPath)
		if got != c.want {
			t.Errorf("resolveExportInput(%q, %q) = %q, want %q",
				c.inputPath, c.defaultLogPath, got, c.want)
		}
	}
}

func TestResolveExportInputEmptyUsesDefault(t *testing.T) {
	got := resolveExportInput("", "trace_output.jsonl")
	if got != "trace_output.jsonl" {
		t.Errorf("empty input: got %q, want trace_output.jsonl", got)
	}
}

func TestResolveExportInputNonEmptyIgnoresDefault(t *testing.T) {
	got := resolveExportInput("custom.jsonl", "trace_output.jsonl")
	if got != "custom.jsonl" {
		t.Errorf("custom input: got %q, want custom.jsonl", got)
	}
}
