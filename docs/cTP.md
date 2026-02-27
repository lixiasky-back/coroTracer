# 📝 cTP (coroTracer Protocol) Memory Layout and Concurrency Synchronization Specification

**Version**: 1.0
**Status**: Production-Ready
**Core Features**: Cross-Language, Zero-Copy, Lock-Free, Cache-Line Friendly

---

## 1. Overview

cTP (coroTracer Protocol) is not a traditional TCP/UDP-based network communication protocol, but rather a **physical memory mapping (mmap) contract strictly based on byte alignment**.

Due to the extreme performance demands of modern M:N coroutine schedulers, traditional RPC or Socket log collection solutions introduce intolerable serialization and context switching overhead. The cTP protocol, by strictly dictating the binary layout and atomic barrier (Memory Barriers) rules of the shared memory (`/tmp/corotracer.shm`), enables the tested target program (C++, Rust, Zig, etc.) to record timing at speeds approaching the L1 Cache, while the Go engine harvests non-blockingly in a completely independent process.

---

## 2. Macro Topology

The entire shared memory file is strictly divided into fixed-size memory blocks. The first 1KB is dedicated to global state negotiation, followed by N consecutive 1KB coroutine observation stations (Station).

```text
[ Shared Memory File: corotracer.shm ]
=======================================================================
| Offset (Hex) | Size (Bytes) | Block Name                            |
=======================================================================
| 0x00000000   | 1024 (1KB)   | GlobalHeader                          |
| 0x00000400   | 1024 (1KB)   | StationData #0                        |
| 0x00000800   | 1024 (1KB)   | StationData #1                        |
| ...          | ...          | ...                                   |
| Header + N*1K| 1024 (1KB)   | StationData #N                        |
=======================================================================
```
*Mandatory Constraint: When implementing this protocol in any language, the total size of the structure must be strictly guaranteed to be exactly 1024 bytes, completely rejecting the compiler's implicit Padding, to ensure absolute cross-language ABI consistency.*

---

## 3. Micro Layout

### 3.1 GlobalHeader (Global Negotiation Header)
**Alignment Requirement**: 1024 Bytes ( `alignas(1024)` )
**Responsibility**: Stores cross-process handshake information and the global cursor for the lock-free allocator.

| Offset | Field | Type | Bytes | Description |
| :--- | :--- | :--- | :--- | :--- |
| `0x00` | `magic_number` | `uint64` | 8 | Magic number, fixed at `0x434F524F54524352` (ASCII: COROTRCR) |
| `0x08` | `version` | `uint32` | 4 | Protocol version number, currently `1` |
| `0x0C` | `max_stations` | `uint32` | 4 | Maximum total number of Stations pre-allocated in the SHM file |
| `0x10` | `allocated_count` | `atomic<uint32>` | 4 | **[Lock-Free Allocator Cursor]** The target program obtains an available Station via atomic increment |
| `0x14` | `tracer_sleeping` | `atomic<uint32>` | 4 | Engine sleep flag: `0` = Active, `1` = Sleeping awaiting wakeup |
| `0x18` | `_reserved` | `char[1000]` | 1000 | **Hard Padding Zone**: Pad to a full 1024 bytes |

### 3.2 Epoch (Core Event Slot)
**Alignment Requirement**: 64 Bytes ( `alignas(64)` )
**Responsibility**: Records a snapshot of a single coroutine state transition.
*Design Philosophy*: 64 bytes perfectly matches the Cache Line size of modern CPUs. When multiple threads concurrently write to different Epochs, they are physically isolated in different cache lines, completely eliminating the drastic performance drops caused by **False Sharing**.

| Offset | Field | Type | Bytes | Description |
| :--- | :--- | :--- | :--- | :--- |
| `0x00` | `timestamp` | `uint64` | 8 | Nanosecond-level timestamp (e.g., `clock_gettime(CLOCK_MONOTONIC)`) |
| `0x08` | `tid` | `uint64` | 8 | Real OS thread ID (not high-level language level ID) |
| `0x10` | `addr` | `uint64` | 8 | Instruction address or coroutine heap frame pointer upon suspension/resumption |
| `0x18` | `seq` | `atomic<uint64>` | 8 | **[Core Concurrency Barrier]** Monotonically increasing sequence number. Used for read/write barriers |
| `0x20` | `reserved` | `char[31]` | 31 | Reserved space (can be used to store a small amount of business Payload) |
| `0x3F` | `is_active` | `bool (uint8)`| 1 | State machine flag: `1` = Active (Running), `0` = Suspend (Suspended) |

### 3.3 StationData (Coroutine Station)
**Alignment Requirement**: 1024 Bytes ( `alignas(1024)` )
**Responsibility**: Each coroutine instance exclusively occupies one Station throughout its entire lifecycle.

| Offset | Zone | Bytes | Description |
| :--- | :--- | :--- | :--- |
| `0x000` | `Header.probe_id` | 8 | Probe globally unique ID (recommended to use the memory address at coroutine creation) |
| `0x008` | `Header.birth_ts` | 8 | Nanosecond timestamp of coroutine birth |
| `0x010` | `Header.is_dead` | 1 | Whether the coroutine has finished destruction (`1` = Dead) |
| `0x011` | `Header._pad` | 47 | Pad to 64-byte alignment |
| `0x040` | `Slots[8]` | 512 | **Event Polling Buffer (RingBuffer)**: 8 Epochs, totaling 512 Bytes |
| `0x240` | `Flexible` | 448 | **Hard Padding Zone**: Pad to a full 1024 bytes |

---

## 4. Concurrency Synchronization and Read/Write Contract

cTP completely abandons Mutex and SpinLock, relying solely on hardware-level memory barriers. Implementing this protocol must comply with the following read/write contract:

### 4.1 Probe Write Side (Target App / SDK)
1. **O(1) Lock-Free Allocation**: When a new coroutine is born, execute `index = fetch_add(&GlobalHeader.allocated_count, 1, std::memory_order_relaxed)`. If `index < max_stations`, exclusively occupy `StationData[index]`.
2. **Circular Write (Ring Buffer)**: Upon context switch, obtain the auto-incremented sequence number `seq`. Locate the slot: `slot = Station.Slots[seq % 8]`.
3. **Memory Barrier [Fatal Constraint]**:
   The probe must **first** write ordinary data such as `timestamp`, `tid`, `addr`, `is_active`.
   As the **final step**, it must update `seq` using `Release` semantics:
   ```cpp
   slot.seq.store(current_seq, std::memory_order_release);
   ```
   This ensures that when the Go engine sees `seq` updated, all preceding data has been flushed to physical memory, absolutely preventing dirty reads.

### 4.2 Engine Harvest Side (Go Tracer Engine)
1. **Local Snapshot**: The Go engine maintains a `last_seen_seqs[MAX_STATIONS][8]` array locally.
2. **Safe Read (Acquire)**: When polling `seq`, atomic loading must be used:
   ```go
   currentSeq := atomic.LoadUint64(&slot.Seq) // Inherently carries an Acquire barrier by default
   ```
3. **Data Extraction**: If `currentSeq > last_seen_seqs`, extract the data of the current slot, and upon completion, update the local `last_seen_seqs`.

### 4.3 Smart Wakeup Contract (UDS Wakeup)
To prevent the Go engine from spinning the CPU idly (Busy Wait) during business troughs, a UDS wakeup mechanism is introduced:
1. After N consecutive harvests with no data, the Go engine sets `GlobalHeader.tracer_sleeping` to `1`, and subsequently blocks reading the UDS (Unix Domain Socket).
2. After writing data, if the C++ probe detects `tracer_sleeping == 1`, it sends a single-byte signal `'1'` to the UDS (using non-blocking `O_NONBLOCK` write; failures are directly ignored, absolutely never blocking the target program).
3. Upon receiving the signal, the Go engine is instantly awakened by the kernel, resets `tracer_sleeping` to `0`, and enters the next round of frantic harvesting.

---

## 5. Cross-Language Implementation Reference (FFI Guide)

> Note: A Rust SDK will be provided later, aiming to make the experience as close as possible to the minor changes required for C++. Other languages are pending (e.g., Zig is currently unstable).

### Rust Language Implementation Mapping Reference (Pseudocode)
In Rust, `#[repr(C)]` and `#[repr(align(X))]` must be strictly used.
```rust
use std::sync::atomic::{AtomicU64, AtomicU32};

#[repr(C, align(64))]
pub struct Epoch {
    pub timestamp: u64,
    pub tid: u64,
    pub addr: u64,
    pub seq: AtomicU64,
    pub reserved: [u8; 31],
    pub is_active: bool,
}

#[repr(C, align(1024))]
pub struct StationData {
    pub probe_id: u64,
    pub birth_ts: u64,
    pub is_dead: bool,
    pub _pad: [u8; 47],
    pub slots: [Epoch; 8],
    pub flexible: [u8; 448],
}
```