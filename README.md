# coroTracer: Cross-language, zero-copy coroutine observability

![Go Engine](https://img.shields.io/badge/Engine-Go_1.21+-00ADD8.svg)
![SDK C++](https://img.shields.io/badge/SDK-C++20-blue.svg)
![Arch](https://img.shields.io/badge/Arch-Language_Agnostic-orange.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)
![zread](https://img.shields.io/badge/Ask_Zread-_.svg?style=flat&color=00b0aa&labelColor=000000&logo=data%3Aimage%2Fsvg%2Bxml%3Bbase64%2CPHN2ZyB3aWR0aD0iMTYiIGhlaWdodD0iMTYiIHZpZXdCb3g9IjAgMCAxNiAxNiIgZmlsbD0ibm9uZSIgeG1sbnM9Imh0dHA6Ly93d3cudzMub3JnLzIwMDAvc3ZnIj4KPHBhdGggZD0iTTQuOTYxNTYgMS42MDAxSDIuMjQxNTZDMS44ODgxIDEuNjAwMSAxLjYwMTU2IDEuODg2NjQgMS42MDE1NiAyLjI0MDFWNC45NjAxQzEuNjAxNTYgNS4zMTM1NiAxLjg4ODEgNS42MDAxIDIuMjQxNTYgNS42MDAxSDQuOTYxNTZDNS4zMTUwMiA1LjYwMDEgNS42MDE1NiA1LjMxMzU2IDUuNjAxNTYgNC45NjAxVjIuMjQwMUM1LjYwMTU2IDEuODg2NjQgNS4zMTUwMiAxLjYwMDEgNC45NjE1NiAxLjYwMDFaIiBmaWxsPSIjZmZmIi8%2BCjxwYXRoIGQ9Ik00Ljk2MTU2IDEwLjM5OTlIMi4yNDE1NkMxLjg4ODEgMTAuMzk5OSAxLjYwMTU2IDEwLjY4NjQgMS42MDE1NiAxMS4wMzk5VjEzLjc1OTlDMS42MDE1NiAxNC4xMTM0IDEuODg4MSAxNC4zOTk5IDIuMjQxNTYgMTQuMzk5OUg0Ljk2MTU2QzUuMzE1MDIgMTQuMzk5OSA1LjYwMTU2IDE0LjExMzQgNS42MDE1NiAxMy43NTk5VjExLjAzOTlDNS42MDE1NiAxMC42ODY0IDUuMzE1MDIgMTAuMzk5OSA0Ljk2MTU2IDEwLjM5OTlaIiBmaWxsPSIjZmZmIi8%2BCjxwYXRoIGQ9Ik0xMy43NTg0IDEuNjAwMUgxMS4wMzg0QzEwLjY4NSAxLjYwMDEgMTAuMzk4NCAxLjg4NjY0IDEwLjM5ODQgMi4yNDAxVjQuOTYwMUMxMC4zOTg0IDUuMzEzNTYgMTAuNjg1IDUuNjAwMSAxMS4wMzg0IDUuNjAwMUgxMy43NTg0QzE0LjExMTkgNS42MDAxIDE0LjM5ODQgNS4zMTM1NiAxNC4zOTg0IDQuOTYwMVYyLjI0MDFDMTQuMzk4NCAxLjg4NjY0IDE0LjExMTkgMS42MDAxIDEzLjc1ODQgMS42MDAxWiIgZmlsbD0iI2ZmZiIvPgo8cGF0aCBkPSJNNCAxMkwxMiA0TDQgMTJaIiBmaWxsPSIjZmZmIi8%2BCjxwYXRoIGQ9Ik00IDEyTDEyIDQiIHN0cm9rZT0iI2ZmZiIgc3Ryb2tlLXdpZHRoPSIxLjUiIHN0cm9rZS1saW5lY2FwPSJyb3VuZCIvPgo8L3N2Zz4K&logoColor=ffffff)

![UDSWakeupMechanics.gif](source/UDSWakeupMechanics.gif)

> **Why I built this**: I was dealing with a really annoying bug in my M:N scheduler. Under heavy load, throughput would just flatline to zero. I ran ASAN and TSAN, but they came up empty because no memory was actually corrupted. It turned out to be a "lost wakeup"—coroutines were stuck forever waiting on a closed file descriptor. Traditional tools just can't catch these logical state machine breaks. I wrote coroTracer to track this exact issue down, and it worked.

coroTracer is an out-of-process tracer for M:N coroutine schedulers. It tracks down logical deadlocks, broken state machines, and coroutine leaks.

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
                                                                        | (Append)
                                                                        v
        +-------------------------+      [ DeepDive ]           +---------------+
        | Interactive HTML Portal | <--- analyzer.go ---------  |  trace.jsonl  |
        +-------------------------+      (Heuristics)           +---------------+
```

---

## How it works

The main idea is simple: keep the tracer out of the target process's way.

* **Execution Plane**: The C++/Rust SDK writes state changes directly into pre-allocated shared memory using lock-free data structures.
* **Observation Plane**: A separate Go engine pulls this data in the background to build the topology. No network overhead, zero context switching.

### The Gritty Details

* **cTP Memory Contract**: It runs on `mmap`. We force a strict 1024-byte alignment so different compilers don't mess things up with implicit padding.
* **64-byte Cache Line Alignment**: Event slots match CPU cache lines exactly. This stops multi-threaded false sharing dead in its tracks during concurrent writes.
* **Zero-Copy**: Data moves purely via pointer offsets and hardware atomics. No RPCs, zero serialization.
* **Smart UDS Wakeup**:
    * When the Go engine is idle, it sets a `TracerSleeping` flag in the shared memory.
    * The SDK does a quick atomic load to check this flag before writing.
    * It only fires a 1-byte Unix Domain Socket (UDS) signal to wake the engine *if* it's actually asleep. This prevents syscall storms when throughput is high.

---

## Quick Start

### 1. Spin up the engine

The Go engine handles the SHM/UDS allocation and starts your app.

```bash
# Build the tracer
go build -o coroTracer main.go

# Run it
./coroTracer -n 256 -cmd "./your_target_app" -out trace.jsonl
```

### 2. Drop in the SDK (C++20 Example)

Your app grabs the IPC config automatically from environment variables.

```cpp
#include "coroTracer.h"

int main() {
    corotracer::InitTracer(); // Sets up mmap and connections
    // ... start your scheduler
}
```

Inherit `PromiseMixin` to hook the lifecycle:

```cpp
struct promise_type : public corotracer::PromiseMixin {
    // Your code here. coroTracer handles await_suspend / await_resume under the hood.
};
```

### 3. Get the reports

```bash
# Markdown report (auto-detects SIGBUS and lost wakeups)
./coroTracer -deepdive -out trace.jsonl

# Interactive HTML dashboard
./coroTracer -html -out trace.jsonl
```

---

## Catching a "Lost Wakeup"

When I was testing my [tiny_coro](https://github.com/lixiasky-back/tiny_coro-build_your_own_MN_scheduler) scheduler, it kept freezing under heavy load. Throughput dropped to zero, but the sanitizers said everything was fine.

I attached coroTracer, and the report showed exactly 47 coroutines permanently stuck in a `Suspended` state. Their instruction pointers were all parked at `co_await AsyncRead(fd)`.

**What went wrong:**
During a massive spike of `EOF/RST` events, the worker thread correctly called `close(fd)`, but it completely missed calling `.resume()` for the coroutines tied to that descriptor. The socket was gone, but the state machine logic was broken. Those coroutines were just stranded in the heap, waiting for a wakeup that would never happen.

* Raw trace: [trace.jsonl](example/trace.jsonl)
* Diagnostic report: [coro_report.md](example/coro_report.md)
* Dashboard preview:
  ![Dashboard](example/coro_dashboard.png)

---

## cTP Memory Layout

| Offset | Field | Size | Description |
| :--- | :--- | :--- | :--- |
| `0x00` | `MagicNumber` | 8B | `0x434F524F54524352` |
| `0x14` | `SleepFlag` | 4B | Engine sleep flag (1 = Sleeping) |
| `0x40` | `EventSlots` | 512B | 8 ring buffers aligned to 64B |

> Full spec in [cTP.md](docs/cTP.md)

---

## Language Support
Right now, I've provided a C++20 SDK. But since the core just relies on a strict memory mapping contract, you can easily write a probe for Rust, Zig, or C—basically anything that supports `mmap`.

> Contact: lixia.chat@outlook.com