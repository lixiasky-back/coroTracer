# 🔬 mini_redis Debug Report: Tracking "Phantom Coroutines" and State Machine Fractures

## 📑 1. Executive Summary
* **Target System**: [`mini_redis`](../src/mini_redis.cpp) (An M:N asynchronous network framework based on C++20 coroutines)
* **Stress Test Conditions**: `redis-benchmark -c 50 -n 10000` (Instantaneous 50 concurrent connections)
* **Symptoms**: The benchmarking client reports `Connection reset by peer`, the server console exhibits multi-thread log interleaving (`30[Client Disconnected] fd:`), and subsequently, system throughput drops to zero.
* **Troubleshooting Challenges**: Enabling ASAN (AddressSanitizer) and TSAN (ThreadSanitizer) yields no errors. Traditional memory analysis tools are completely ineffective.
* **Diagnostic Tool**: Self-developed lock-free high-precision tracer, `coroTracer`.
* **Root Cause**: **State Machine Fracture** leading to collective coroutine leakage and logical deadlocks, rather than memory corruption/trampling.

---

## 🛑 2. Eliminating False Positives: The 0x0 False Alarm
In the initial DeepDive report, there were numerous suspected `SIGBUS` warnings targeting the `0x0000000000000000` address.
After code tracing, it was confirmed that this is **not** a null pointer dereference.
* **The Truth**: This occurred because in the C++ probe code, when the coroutine is normally awakened by the scheduler (`await_resume`), it hardcoded the call `tracer->write_trace(0, true)`.
* **Conclusion**: These `0x0` trace points are actually extremely healthy "vital signs" of the coroutines, proving that the underlying 1024-byte alignment fix works perfectly and there is no dirty data in the shared memory.

---

## 🕵️‍♂️ 3. Hard Evidence: Digging into the "Phantom List"
After filtering out the noise, the true culprit behind the crash surfaced—**47 phantom coroutines that could never be awakened (Lost Wakeups)**. By combining tracer data and terminal logs, the crime scene was perfectly reconstructed:

### Evidence A: The Perfect Concurrency Funnel (50 -> 47)
The stress testing tool instantly initiated **50** TCP connections, yet exactly **47** permanently suspended coroutines were left on the tracer dashboard. This implies that while handling the instantaneous high-concurrency I/O storm, the underlying Reactor (kqueue/epoll) captured the events but only successfully processed a tiny fraction; the remaining 47 execution flows aborted mid-way.

### Evidence B: The Runaway Four-Cylinder Engine (Worker Pool)
Observing the last executing thread IDs (TID) before suspension:
`1297297`, `1297298`, `1297299`, `1297300`
These 4 consecutive system thread IDs prove that `mini_redis` utilizes a 4-thread Worker Pool model. When a massive number of clients disconnected abnormally, these 4 threads were awakened simultaneously. Lock-free contention led to character interleaving in the standard output stream (e.g., `[Client Disconnected] fd: [Client Disconnected] fd: 42`).

### Evidence C: Highly Clustered "Death Spots" (Address Clustering)
The instruction addresses of these 47 coroutines prior to suspension are highly converged, all located in the `0x0000000b394...` memory segment. This indicates they did not randomly deadlock while executing complex business logic, but rather **collectively got stuck on the underlying instruction of the exact same line of code** (highly likely the suspension/yield point of `co_await AsyncRead(fd)`).

---

## 🧩 4. Crime Scene Reconstruction: A Perfect Logical Murder
Based on the evidence above, the paralysis process of `mini_redis` unfolded as follows:

1. **Connection Establishment**: 50 connections surge in, and the scheduler allocates 50 coroutines. The coroutines execute up to `co_await AsyncRead(fd)`, suspend themselves, and hand over the `fd` to the underlying kqueue/epoll for management.
2. **Connection Anomaly**: Due to the benchmark stress testing strategy or overly rapid transmission, a massive number of connections triggered TCP `RST` or `EOF`.
3. **Scheduler Amnesia**: 4 Worker threads wake up from epoll and discover these disconnected `fd`s. They dutifully print `[Client Disconnected]` and call `close(fd)`.
4. **The Fatal Omission (Root Cause)**: **When calling `close(fd)`, the scheduler forgot to awaken the coroutine bound to that `fd`!**
5. **Birth of the Phantoms**: The underlying Socket has been destroyed, and the OS will no longer push any events. Meanwhile, the 47 upper-layer coroutines remain permanently in the `Suspend` state because `handle.resume()` was never called. The heap memory containing the coroutine frames cannot be freed, causing irreversible logical deadlocks and memory leaks.

---

## 🛡️ 5. Why Were ASAN and TSAN Blinded?
This is exactly the most fascinating part of this debugging session—revealing the blind spots of traditional Sanitizer tools:
* **Reason for ASAN (AddressSanitizer) Failure**: The contexts of these 47 coroutines were safely stored in heap memory. The program simply "forgot" to execute them, and no thread attempted out-of-bounds reads/writes or illegal freeing of this memory. To ASAN, this was just an ordinary "memory leak" that hadn't yet triggered an OOM.
* **Reason for TSAN (ThreadSanitizer) Failure**: Because the coroutines were completely forgotten, no thread concurrently accessed their local variables. Without access, there naturally is no data race.

Faced with this purely **asynchronous state machine fracture**, only `coroTracer`, based on global chronological and state topology tracking, could serve as the ultimate weapon to break the deadlock.

---

## 🛠️ 6. Remediation Recommendations (Action Items)
1. **Refactor Event Loop Disconnect Logic**:
   In the kqueue/epoll event processing loop, upon detecting `EV_EOF`, `EPOLLERR`, or `EPOLLHUP`, you **must** locate the corresponding coroutine handle from the wait queue and call `handle.resume()` either before or after calling `close(fd)`.
2. **Enhance Coroutine Error Handling**:
   Ensure that abnormally awakened coroutines can properly perceive network errors (e.g., `read()` returning 0 or -1), thereby gracefully exiting the business logic and executing `co_return` to complete the lifecycle closure and memory reclamation.
3. **Optimize Diagnostic Scripts**:
   Remove the false positive `SIGBUS` rules for the `0x0` address in the `DeepDive` analysis scripts, or add contextual filtering logic.