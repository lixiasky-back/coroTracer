# coroTracer CLI Usage Guide

This document explains every command-line flag currently supported by `coroTracer`, including:

- what it does
- its default value
- which mode it belongs to
- common flag combinations
- the most common points of confusion

Program entry point:

- [main.go](../main.go)

---

## 1. The Two Operating Modes

`coroTracer` currently has exactly two modes, and they are **strictly mutually exclusive**.

### Trace Collection Mode

Triggered by `-cmd`.

This mode will:

- create shared memory
- create the Unix Domain Socket
- launch the target program
- harvest coroutine events
- write JSONL output

Minimal example:

```bash
./coroTracer -cmd "./your_target_app"
```

### Export Mode

Triggered by `-export`.

This mode will:

- read an already existing JSONL trace
- convert it into SQLite / MySQL / PostgreSQL / CSV

Minimal example:

```bash
./coroTracer -export sqlite -in trace.jsonl
```

### Mutual Exclusion

This combination is **not allowed**:

```bash
./coroTracer -cmd "./your_target_app" -export sqlite
```

The current design is:

- `-cmd` produces JSONL
- `-export` consumes an existing JSONL

They are separate stages, not a single chained run.

---

## 2. Flag Summary

| Flag | Default | Mode | Purpose |
| --- | --- | --- | --- |
| `-n` | `128` | trace | preallocated station count |
| `-cmd` | empty | trace | target command to launch and trace |
| `-shm` | `/tmp/corotracer.shm` | trace | shared memory file path |
| `-sock` | `/tmp/corotracer.sock` | trace | UDS path |
| `-out` | `trace_output.jsonl` | trace | JSONL output path |
| `-export` | empty | export | export target type |
| `-in` | empty | export | input JSONL path; falls back to `-out` |
| `-sqlite-out` | empty | export | SQLite output path; defaults to `<input>.sqlite` |
| `-csv-out` | empty | export | CSV output path; defaults to `<input>.csv` |
| `-db-cli` | empty | export | override the default database CLI name |
| `-db-host` | `127.0.0.1` | export | MySQL / PostgreSQL host |
| `-db-port` | `0` | export | MySQL / PostgreSQL port; inferred by exporter type |
| `-db-user` | empty | export | database user |
| `-db-password` | empty | export | database password |
| `-db-name` | `coro_tracer` | export | database name |
| `-db-table` | `coro_trace_events` | export | table name |
| `-mysql-socket` | empty | export | MySQL Unix socket path |
| `-pg-maintenance-db` | `postgres` | export | PostgreSQL maintenance database used for `CREATE DATABASE` |
| `-pg-sslmode` | empty | export | value forwarded through `PGSSLMODE` |

---

## 3. Trace Collection Flags

### `-n`

Default:

```text
128
```

Purpose:

- sets how many stations are preallocated
- effectively defines the current upper collection capacity for coroutine tracking

Important note:

- one of the current known limitations is that capacity is still **fixed and finite**
- it is not dynamically growing
- if your coroutine population is substantially larger, you should raise this value explicitly

Example:

```bash
./coroTracer -n 512 -cmd "./your_target_app"
```

### `-cmd`

Default:

```text
empty
```

Purpose:

- specifies the target command that `coroTracer` should launch and trace

Implementation detail:

- internally the program uses `sh -c`
- so commands with arguments work directly

Example:

```bash
./coroTracer -cmd "./server --threads 4 --config ./conf.yaml"
```

Important:

- once `-cmd` is present, the program is in trace mode
- you may not also provide `-export`

### `-shm`

Default:

```text
/tmp/corotracer.shm
```

Purpose:

- sets the shared-memory file path

Useful when:

- running multiple test instances in parallel
- the default path collides with another run
- you want the IPC files in a custom location

Example:

```bash
./coroTracer -cmd "./your_target_app" -shm /tmp/case1.shm
```

### `-sock`

Default:

```text
/tmp/corotracer.sock
```

Purpose:

- sets the UDS wakeup path

Useful when:

- running multiple instances in parallel
- the default path collides with something else

Example:

```bash
./coroTracer -cmd "./your_target_app" -sock /tmp/case1.sock
```

### `-out`

Default:

```text
trace_output.jsonl
```

Purpose:

- sets the JSONL output file produced by trace collection

Example:

```bash
./coroTracer -cmd "./your_target_app" -out traces/run1.jsonl
```

Extra note:

- in export-only mode, if `-in` is omitted, the program falls back to the value of `-out`

---

## 4. Export Mode Flags

### `-export`

Default:

```text
empty
```

Purpose:

- selects the export type

Currently supported values:

- `sqlite`
- `mysql`
- `postgres`
- `postgresql`
- `dataframe`
- `csv`

Notes:

- `postgres` and `postgresql` are equivalent
- `dataframe` and `csv` are equivalent and both export CSV

### `-in`

Default:

```text
empty
```

Purpose:

- sets the input JSONL file in export mode

Default behavior:

- if `-in` is omitted, the program falls back to `-out`

For example:

```bash
./coroTracer -export sqlite -out trace.jsonl
```

is effectively the same as:

```bash
./coroTracer -export sqlite -in trace.jsonl
```

In practice, using `-in` explicitly is clearer.

---

## 5. SQLite Export Flag

### `-sqlite-out`

Default:

```text
empty
```

Purpose:

- sets the SQLite database output path

Default behavior:

- if omitted, the program derives `<input>.sqlite`

Example:

```bash
./coroTracer -export sqlite -in trace.jsonl -sqlite-out out/trace.sqlite
```

Runtime dependency:

- a local `sqlite3` binary is required

---

## 6. CSV / DataFrame Export Flag

### `-csv-out`

Default:

```text
empty
```

Purpose:

- sets the CSV output path

Default behavior:

- if omitted, the program derives `<input>.csv`

Example:

```bash
./coroTracer -export csv -in trace.jsonl -csv-out out/trace.csv
```

Good downstream targets:

- pandas
- polars
- DuckDB
- R

---

## 7. Common MySQL / PostgreSQL Flags

### `-db-cli`

Purpose:

- overrides the default database CLI command name

Default behavior:

- MySQL export uses `mysql`
- PostgreSQL export uses `psql`

### `-db-host`

Default:

```text
127.0.0.1
```

Purpose:

- sets the MySQL / PostgreSQL host

### `-db-port`

Default:

```text
0
```

Actual default behavior:

- for MySQL export, `0` becomes `3306`
- for PostgreSQL export, `0` becomes `5432`

### `-db-user`

Purpose:

- sets the database user

### `-db-password`

Purpose:

- sets the database password

Notes:

- this is intended for the user's own database password
- MySQL forwards it through `MYSQL_PWD`
- PostgreSQL forwards it through `PGPASSWORD`

Security note:

- a plaintext password may appear in shell history

### `-db-name`

Default:

```text
coro_tracer
```

Purpose:

- sets the database name

### `-db-table`

Default:

```text
coro_trace_events
```

Purpose:

- sets the destination table name

---

## 8. MySQL-Specific Flag

### `-mysql-socket`

Default:

```text
empty
```

Purpose:

- connects to MySQL over a Unix socket

Once set:

- `-db-host` and `-db-port` are ignored

Example:

```bash
./coroTracer \
  -export mysql \
  -in trace.jsonl \
  -db-user root \
  -db-password your_password \
  -mysql-socket /tmp/mysql.sock
```

Runtime dependency:

- a local `mysql` CLI is required

---

## 9. PostgreSQL-Specific Flags

### `-pg-maintenance-db`

Default:

```text
postgres
```

Purpose:

- when the target database does not exist, the program first connects to this maintenance database
- then it attempts `CREATE DATABASE`

### `-pg-sslmode`

Default:

```text
empty
```

Purpose:

- forwards a value through `PGSSLMODE` to `psql`

Common values:

- `disable`
- `require`
- `verify-ca`
- `verify-full`

Example:

```bash
./coroTracer \
  -export postgresql \
  -in trace.jsonl \
  -db-user postgres \
  -db-password your_password \
  -pg-sslmode disable
```

Runtime dependency:

- a local `psql` CLI is required

---

## 10. Common Command Combinations

### Minimal trace run

```bash
./coroTracer -cmd "./your_target_app"
```

### Trace with explicit capacity and output path

```bash
./coroTracer -n 512 -cmd "./your_target_app --threads 4" -out traces/run1.jsonl
```

### Export to SQLite

```bash
./coroTracer -export sqlite -in traces/run1.jsonl
```

### Export to CSV

```bash
./coroTracer -export csv -in traces/run1.jsonl
```

### Export to MySQL

```bash
./coroTracer \
  -export mysql \
  -in traces/run1.jsonl \
  -db-host 127.0.0.1 \
  -db-port 3306 \
  -db-user root \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events
```

### Export to PostgreSQL

```bash
./coroTracer \
  -export postgresql \
  -in traces/run1.jsonl \
  -db-host 127.0.0.1 \
  -db-port 5432 \
  -db-user postgres \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events \
  -pg-sslmode disable
```

---

## 11. Common Misunderstandings

### Can `-cmd` and `-export` be used together?

No. They are mutually exclusive modes.

### Is `-n` a thread count?

No. It is much closer to the upper collection capacity for coroutine tracking.

### What happens if `-in` is omitted?

In export mode the program falls back to the value of `-out`.

### Why use system CLIs for database export instead of Go drivers?

That is the current implementation tradeoff. The goal is to keep the repository's Go dependency set lightweight.

---

## 12. Related Documents

- [README.md](../README.md)
- [README_ch.md](../README_ch.md)
- [docs/cTP.md](cTP.md)
- [proof.md](../proof.md)
- [proof_en.md](../proof_en.md)
