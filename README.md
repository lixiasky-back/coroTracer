# 🔬 coroTracer: Cross-Language, Zero-Copy Coroutine Observability

![Go Engine](https://img.shields.io/badge/Engine-Go_1.21+-00ADD8.svg) ![SDK C++](https://img.shields.io/badge/SDK-C++20-blue.svg) ![Arch](https://img.shields.io/badge/Arch-Language_Agnostic-orange.svg) ![License](https://img.shields.io/badge/license-MIT-green.svg)

**coroTracer** is a high-performance, cross-language, full-link observability foundation for asynchronous coroutines.

In modern high-concurrency M:N coroutine schedulers, "asynchronous state machine fractures," "Lost Wakeups," and "coroutine leaks" are phantom logical deadlocks that are extremely difficult for traditional memory tools (like ASAN/TSAN) to capture.

coroTracer abandons traditional network instrumentation and locking mechanisms, defining an extremely lightweight **cTP (coroTracer Protocol) shared memory layout contract**. Any system-level language that supports `mmap` (C++, Rust, Zig, etc.) can be rapidly integrated as a tested target. The Go engine harvests state transitions in the background with absolute zero blocking, ultimately rendering them into a static topology dashboard free from timeline pollution, leaving Heisenbugs under high pressure nowhere to hide.

---

## 🏗️ System Architecture

The core design philosophy of coroTracer is the **"absolute physical isolation of the observation plane and the execution plane"**. The target program only needs to overwrite its state in memory with extremely low instruction cycles; complex aggregation, persistence, and heuristic diagnostics are all handled asynchronously by the Go engine in a separate process.

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
                                                                        | (Append)
                                                                        v
        +-------------------------+      [ DeepDive ]           +---------------+
        | Interactive HTML Portal | <--- analyzer.go ---------  |  trace.jsonl  |
        +-------------------------+      (Heuristics)           +---------------+
```

---

## 🚀 Core Features

* **Language-Agnostic Observability Contract**: Abandons RPC, utilizing an underlying physical memory contract (cTP) strictly aligned to 1024 bytes for cross-process communication. (👉 [See cTP Memory Protocol Specification for details](docs/cTP_Protocol.md))
* **Extreme Zero-Copy**: The probe writes to physical memory, and the Go engine reads from physical memory, achieving zero serialization overhead across the entire link.
* **Lock-Free Harvesting Engine**:
    * **Cache Line Immunity**: Event slots are strictly aligned to 64 bytes, completely eliminating multi-threaded false sharing.
    * **One-Way Atomic Snapshot**: The Go side consumes data via cursor comparison based on hardware-level atomic instructions, while the target program simply runs at full speed.
* **Smart Wakeup Scheduling**: The engine sleeps with zero CPU consumption when idle, and the target program triggers an ultra-fast wakeup via a non-blocking Unix Domain Socket (UDS).
* **Physically Isolated Topology Dashboard**: Abandons the global timeline, compiling each coroutine instance into an independent interactive HTML tab, completely blocking the visual pollution of erroneous data.

---

## 🛠️ Quick Start

### Step 1: Launch the Observability Engine (Launcher)
The engine pre-allocates shared memory and the UDS Socket, and is responsible for pulling up your target program.

```bash
# Compile the observability engine
go build -o coroTracer main.go

# Start the engine and launch the target program
# -n   : Number of pre-allocated concurrent coroutine slots (default 128)
# -cmd : The target program and arguments to be executed and observed
# -out : Trace trajectory output file (default trace_output.jsonl)
./coroTracer -n 256 -cmd "./your_target_app" -out trace.jsonl
```

### Step 2: Integrate the SDK (Using C++20 as an example)
The launched target program will automatically inherit the necessary IPC environment variables. Complete the initialization at the business entry point:

```cpp
#include "coroTracer.h"

int main() {
    // Automatically read environment variables, complete mmap memory mapping and UDS connection
    corotracer::InitTracer(); 
    // ... Start the business scheduler
}
```

Make the coroutine `promise_type` inherit `PromiseMixin` to seamlessly intercept its lifecycle:
```cpp
struct promise_type : public corotracer::PromiseMixin {
    // Business code... coroTracer will automatically hijack await_suspend / await_resume
};
```

### Step 3: Generate DeepDive Report and Dashboard
After the stress test ends or a deadlock occurs, use the engine to analyze the trace file:

```bash
# Generate Markdown-formatted in-depth diagnostic report (automatically detects SIGBUS and Lost Wakeups)
./coroTracer -deepdive -out trace.jsonl

# Compile the interactive HTML topology dashboard (recommended)
./coroTracer -html -out trace.jsonl
```

---

## 📖 Case Study Highlight: Capturing "Phantom Coroutines"

> **"When both ASAN and TSAN remain silent, how do you find the Heisenbug that reduces throughput to zero?"**

While developing `mini_redis`, a C++20 network framework based on `kqueue`, we encountered a devastating pseudo-death bug under high-pressure concurrency. Google's Sanitizer tools gave green lights all around (no memory out-of-bounds, no data races).

After mounting **coroTracer**, the `DeepDive` report instantly exposed the "perfect crime" scene:
**47 coroutines fell into an eternal suspended state (Lost Wakeup)**, and the instructions before suspension all pointed to `co_await AsyncRead(fd)`.

**Case Review:**
A momentary flood of connections triggered massive underlying `EOF/RST` events. The scheduler's Worker thread was awakened and abruptly executed `close(fd)`, but **forgot to call `.resume()` on the coroutines bound to that fd**! The underlying `fd` had vanished into thin air, but the logical state machines of 47 coroutines were completely fractured, permanently trapped in heap memory, waiting for a wakeup signal that would never arrive.

Faced with this purely logical fracture in the asynchronous state machine, coroTracer relied on its nanosecond-level topological timing tracing to forcefully drag these 47 forgotten phantom coroutines out of the void.

---
---

# 📝 coroTracer Protocol (cTP) Memory Layout Specification
*(Suggested save path: `docs/cTP_Protocol.md`)*

cTP is the core foundation for coroTracer to achieve cross-language, zero-copy observability. It is not a network protocol, but rather a **physical memory mapping (mmap) contract strictly based on byte alignment**.

Any language (C++, Rust, Zig, C) that can define equivalent structures in memory according to the following specifications can seamlessly integrate into the coroTracer engine as a tested target.

## 1. Macroscopic Memory Topology

The entire shared memory file (`/tmp/corotracer.shm`) is divided into two major parts: **1 GlobalHeader** and **an array of N StationData**.

```text
[ Shared Memory File ]
+-------------------------------------------------------------+
| GlobalHeader (Offset: 0, Size: 1024 Bytes)                  |
+-------------------------------------------------------------+
| StationData #0 (Offset: 1024, Size: 1024 Bytes)             |
+-------------------------------------------------------------+
| StationData #1 (Offset: 2048, Size: 1024 Bytes)             |
+-------------------------------------------------------------+
| ... StationData #N ...                                      |
+-------------------------------------------------------------+
```
*(Note: To completely avoid the implicit Padding behavior of different language compilers, the sizes of the Header and Station are hard-locked at 1024 bytes.)*

## 2. Structure Definition Reference (Using C/C++ layout as an example)

### GlobalHeader (1024 Bytes)
Located at the very beginning of the shared memory, used for basic state negotiation between processes.
```cpp
struct alignas(1024) GlobalHeader {
    uint64_t magic_number;       // 0x00: Must be 0x434F524F54524352
    uint32_t version;            // 0x08: Protocol version number (1)
    uint32_t max_stations;       // 0x0C: Pre-allocated maximum number of coroutine slots
    std::atomic<uint32_t> allocated_count; // 0x10: Lock-free incremental allocator on the probe side
    std::atomic<uint32_t> tracer_sleeping; // 0x14: Engine sleep flag (1=Sleeping)
    char _reserved[1000];        // 0x18: Pad to 1024 bytes
};
```

### StationData (1024 Bytes)
Represents an independent lifecycle observation station for a single coroutine. It contains header information and an 8-slot RingBuffer internally.
```cpp
struct alignas(1024) StationData {
    struct {
        uint64_t probe_id;       // 0x00: Coroutine unique identifier (usually heap memory address)
        uint64_t birth_ts;       // 0x08: Creation timestamp (nanoseconds)
        bool is_dead;            // 0x10: Whether the coroutine is destroyed
        char _pad[47];           // 0x11: Pad to 64 bytes
    } header;
    
    Epoch slots[8];              // 0x40: 8 buffer slots (8 * 64 = 512 Bytes)
    
    char flexible[448];          // 0x240: Pad to 1024 bytes (reserved for extension)
};
```

### Epoch Event Slot (64 Bytes)
**Extremely core design**: The size is strictly set to 64 bytes, perfectly matching the Cache Line size of modern CPUs, completely avoiding False Sharing performance loss during concurrent writing.
```cpp
struct alignas(64) Epoch {
    uint64_t timestamp;          // 0x00: Nanosecond-level timestamp
    uint64_t tid;                // 0x08: Real OS thread ID
    uint64_t addr;               // 0x10: Instruction address / Context pointer
    std::atomic<uint64_t> seq;   // 0x18: Sequence number, must be written (Release) and read (Acquire) via atomic
    char reserved[31];           // 0x20: Reserved
    bool is_active;              // 0x3F: State machine (True=Running, False=Suspended)
};
```

## 3. Concurrency Safety Contract (Lock-Free Write Stream)
1. **Incremental Allocation**: The probe obtains the `AllocatedCount` via the `fetch_add` atomic operation to get its exclusive `StationData` index, achieving lock-free allocation of coroutine slots.
2. **Safe Overwrite**: Since `slots` only has 8 positions, the probe uses `current_seq % 8` for circular overwrite writing.
3. **Seq Barrier**: After populating the other fields of the `Epoch`, the probe must finally update `seq` atomically using `std::memory_order_release`. The Go engine perceives new events by polling changes in `seq`.