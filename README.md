# coroTracer: Cross-Language, Zero-Copy Coroutine Observability

![Go Engine](https://img.shields.io/badge/Engine-Go_1.21+-00ADD8.svg)
![SDK C++](https://img.shields.io/badge/SDK-C++20-blue.svg)
![Arch](https://img.shields.io/badge/Arch-Language_Agnostic-orange.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)

![UDSWakeupMechanics.gif](source/UDSWakeupMechanics.gif)

> **Why I built this**: while debugging one of my own M:N schedulers, I ran into an especially nasty failure mode. Under heavy load, throughput would suddenly collapse to zero, but ASAN and TSAN stayed silent because nothing was corrupt in the usual memory-safety sense. It turned out to be a classic `lost wakeup`: the coroutine had become logically unreachable, but traditional tooling was terrible at surfacing that kind of state-machine break. coroTracer was built for exactly this class of problem.

coroTracer is an **out-of-process** coroutine trace collector.  
It is designed for M:N coroutine schedulers, with a very specific goal:

- capture coroutine state transitions
- minimize interference with the target process
- emit reusable raw traces
- provide a reliable low-level foundation for later offline analysis and database export

It is not positioned as an APM product or an online analysis platform.  
At the moment, this repository is focused on two things:

1. **safely collecting coroutine state into JSONL**
2. **exporting an existing JSONL trace into SQLite / MySQL / PostgreSQL / CSV**

The core safety properties of the collection protocol have also been modeled and proved in Lean 4. Relevant files:

- [proof/proof.lean](./proof/proof.lean)
- [proof.md](./proof/proof.md)
- [proof_en.md](./proof/proof_en.md)
- [docs/cli_usage.md](./docs/cli_usage.md)

> **Project status**: at this point the project is already usable end to end. The collection, persistence, and export pipeline is working as a closed loop. If I had to point out the one remaining obvious limitation, it is that collection capacity is still based on a **fixed finite coroutine count**, rather than a dynamically growing capacity. Aside from that, the project is already usable in practice. Updates will continue, but the pace will likely slow down significantly, probably much more than before.
> This update was focused on data format conversion and export, and did not touch the core collection path. Codex genuinely improved iteration speed a lot here, which helped this release land much faster.

---

## Architecture

```text
+-----------------------+                               +-----------------------+
|   Target Application  |                               |    Go Tracer Engine   |
|  (C++, Rust, Zig...)  |                               |                       |
|                       |       [ Lock-Free SHM ]       |                       |
|  +-----------------+  |      +-----------------+      |  +-----------------+  |
|  |  cTP SDK Probe  |=======> | StationData [N] | <=======|  Harvester Loop   |  |
|  +-----------------+  |  Write +-----------------+ Read |  +-----------------+  |
|                       |               ^               |                       |
|       [ Socket ]      |---(Wakeup)---UDS---(Listen)---|      [ File I/O ]     |
+-----------------------+                               +-----------------------+
                                                                        |
                                                                        v
                                                               +------------------+
                                                               |  trace_output    |
                                                               |     .jsonl       |
                                                               +------------------+
                                                                        |
                                                                        v
                                                          +----------------------------------+
                                                          | SQLite / MySQL / PostgreSQL / CSV |
                                                          +----------------------------------+
```

---

## Current Capabilities

### 1. Trace Collection Mode

The Go engine is responsible for:

- creating shared memory
- creating the Unix Domain Socket
- launching the target process
- continuously harvesting coroutine events from shared memory
- writing the result as JSONL

Each JSONL line looks roughly like this:

```json
{"probe_id":123,"tid":456,"addr":"0x0000000000000000","seq":2,"is_active":true,"ts":123456789}
```

Those fields correspond to the source-level `TraceRecord`:

- `probe_id`: unique coroutine probe identifier
- `tid`: real OS thread ID
- `addr`: suspension address or related coroutine address
- `seq`: slot sequence number
- `is_active`: whether the coroutine is currently active
- `ts`: timestamp

### 2. Export Mode

The repository now includes an `export/` directory that supports converting an **existing JSONL** trace into:

- a SQLite database
- a MySQL database
- a PostgreSQL database
- a DataFrame-friendly CSV file

This is explicitly a **second-stage export** from an existing JSONL trace.  
It is not "trace and write to a database at the same time."

### 3. SDKs

The repository currently ships a C++20 header-only SDK:

- [SDK/c++/coroTracer.h](./SDK/c++/coroTracer.h)

It also now ships a framework-free Rust poll-model SDK:

- [SDK/rust](./SDK/rust)

Their responsibilities are:

- attaching to shared memory
- attaching to the UDS wakeup channel
- writing coroutine state on suspend / resume
- obeying the cTP memory contract

---

## Core Mechanism

The central design idea is simple:

> **physically separate the execution plane from the observation plane.**

The target process only writes state into shared memory.  
The Go collector harvests those states asynchronously from outside the process, instead of pushing complicated tracing logic back into the target.

### 1. Shared Memory Protocol (cTP)

The protocol-level document is here:

- [docs/cTP.md](./docs/cTP.md)

There are three essential ideas:

1. `GlobalHeader` and `StationData` are forced into fixed layouts
2. `Epoch` is aligned to a 64-byte cache line
3. the writer and reader coordinate through a lock-free `seq` discipline

### 2. The C++ Write Protocol

The writer does not simply blast fields into memory without structure.  
It follows a strict order:

1. first make `seq` odd to mark "write in progress"
2. then write the payload
3. finally make `seq` even to mark "write complete"

This corresponds to `PromiseMixin::write_trace` in [SDK/c++/coroTracer.h](./SDK/c++/coroTracer.h).

### 3. The Go Read Protocol

The Go reader also does not trust a slot just because data is present.  
It follows three steps:

1. read `seq` once
2. only if `seq` is even and newer than local `lastSeen` does it copy the payload
3. read `seq` again after the copy
4. only if the two `seq` values match does it write JSONL

This is implemented in:

- [structure/station.go](structure/station.go)
- [structure/jsonl.go](structure/jsonl.go)

### 4. Smart UDS Wakeup

To avoid wasting CPU cycles when traffic is low:

- the Go side sets `TracerSleeping = 1` while idle
- once the C++ side finishes a write and notices the tracer is sleeping, it sends a 1-byte UDS wakeup signal

This avoids syscall storms under heavy throughput while also avoiding a pure busy-spin under light throughput.

---

## Quick Start

### 1. Build

```bash
go build -o coroTracer main.go
```

### 2. Trace a Target Program

```bash
./coroTracer -n 256 -cmd "./your_target_app" -out trace.jsonl
```

This does the following:

- preallocates 256 stations
- launches `./your_target_app`
- writes the trace into `trace.jsonl`

One important constraint:

- `-cmd` mode is collection-only
- it does not export into a database in the same run

So collection and export are **two separate stages**.

### 3. Integrate the C++ SDK

The target program inherits IPC configuration through environment variables.

The smallest possible integration looks like this:

```cpp
#include "coroTracer.h"

int main() {
    corotracer::InitTracer();
    // ... start your scheduler
}
```

For coroutine promises, you can inherit from `PromiseMixin`:

```cpp
struct promise_type : public corotracer::PromiseMixin {
    // your business logic
};
```

The SDK records the state transitions associated with `await_suspend` and `await_resume`.

---

## Exporting JSONL

Export mode only works on an **already existing JSONL file**.  
It cannot be used together with `-cmd`.

So this is allowed:

```bash
./coroTracer -export sqlite -in trace.jsonl
```

But this is not:

```bash
./coroTracer -cmd "./your_target_app" -export sqlite
```

### 1. Export to SQLite

```bash
./coroTracer -export sqlite -in trace.jsonl -sqlite-out trace.sqlite
```

Notes:

- by default the output filename is derived as `<input>.sqlite`
- runtime requires a local `sqlite3` binary

### 2. Export to CSV (DataFrame-Friendly)

```bash
./coroTracer -export csv -in trace.jsonl -csv-out trace.csv
```

That CSV can be consumed directly by:

- pandas
- polars
- DuckDB
- R

### 3. Export to MySQL

```bash
./coroTracer \
  -export mysql \
  -in trace.jsonl \
  -db-host 127.0.0.1 \
  -db-port 3306 \
  -db-user root \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events
```

If you use a Unix socket, you can also do:

```bash
./coroTracer \
  -export mysql \
  -in trace.jsonl \
  -db-user root \
  -db-password your_password \
  -mysql-socket /tmp/mysql.sock
```

Notes:

- runtime requires a local `mysql` CLI
- the exporter creates the database and table automatically, then inserts the data

### 4. Export to PostgreSQL

```bash
./coroTracer \
  -export postgresql \
  -in trace.jsonl \
  -db-host 127.0.0.1 \
  -db-port 5432 \
  -db-user postgres \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events \
  -pg-sslmode disable
```

Notes:

- runtime requires a local `psql` CLI
- the exporter checks whether the target database exists and creates it when needed
- by default it uses `postgres` as the maintenance database; you can override that with `-pg-maintenance-db`

### 5. Common Export Flags

The current export-related flags are:

- `-export`
- `-in`
- `-sqlite-out`
- `-csv-out`
- `-db-cli`
- `-db-host`
- `-db-port`
- `-db-user`
- `-db-password`
- `-db-name`
- `-db-table`
- `-mysql-socket`
- `-pg-maintenance-db`
- `-pg-sslmode`

In particular:

- `-db-password` is intended for the user's own database password
- `-db-cli` overrides the default CLI command name
  - MySQL defaults to `mysql`
  - PostgreSQL defaults to `psql`

For the full parameter reference, see:

- [docs/cli_usage.md](docs/cli_usage.md)

---

## Lean 4 Proof

One of the more important aspects of this project is that the collection protocol is not justified by intuition alone.  
It has been formally modeled.

A good reading order is:

1. [proof/proof.lean](./proof/proof.lean)
2. [proof.md](./proof/proof.md)
3. [proof_en.md](./proof/proof_en.md)

The proof covers the following core properties:

- Go does not commit half-written dirty data into the log
- if the writer leaves a short non-interfering window, Go is guaranteed to complete one successful harvest

The main source-level correspondence is in:

- [SDK/c++/coroTracer.h](./SDK/c++/coroTracer.h)
- [structure/station.go](./structure/station.go)
- [structure/jsonl.go](./structure/jsonl.go)

---

## Current Boundaries

To avoid confusion, here are the current project boundaries.

### 1. This Repository Is Not an Analysis Platform

What it provides today is:

- low-level collection
- JSONL persistence
- export into databases / CSV

It no longer follows the old built-in "report generator / HTML analyzer" direction.

### 2. The Current Focus Is the C++20 / Rust SDKs

Although the protocol itself is language-agnostic, the repository currently ships official SDKs for:

- C++20 coroutine integration
- Rust `Future::poll` integration

Zig and C are still possible in principle because the foundation only depends on:

- `mmap`
- fixed ABI layout
- atomic read/write discipline

### 3. Runtime External Dependencies

If you use export mode, the current implementation depends on local CLI tools:

- SQLite: `sqlite3`
- MySQL: `mysql`
- PostgreSQL: `psql`

This is intentional. It keeps the Go dependency set light and avoids pulling in extra database drivers.

---

## Repository Layout

The most important files and directories right now are:

- [main.go](main.go): entry point, switches between trace mode and export mode
- [engine/engine.go](engine/engine.go): shared memory, UDS, and the hot harvest loop
- [structure/station.go](structure/station.go): core read protocol
- [structure/jsonl.go](structure/jsonl.go): JSONL output
- [export/](export/): SQLite / MySQL / PostgreSQL / CSV export
- [SDK/c++/coroTracer.h](SDK/c++/coroTracer.h): C++20 SDK
- [SDK/rust/](SDK/rust/): Rust poll-model SDK
- [docs/cTP.md](docs/cTP.md): memory protocol documentation
- [proof/proof.lean](./proof/proof.lean): Lean 4 proof
- [proof.md](./proof/proof.md): detailed Chinese proof walkthrough
- [proof_en.md](./proof/proof_en.md): detailed English proof walkthrough

---

## Contact

> lixia.chat@outlook.com
