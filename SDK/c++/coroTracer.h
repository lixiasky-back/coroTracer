#pragma once

#include <coroutine>
#include <atomic>
#include <cstdint>
#include <utility>
#include <iostream>
#include <cstdlib>
#include <cstring>
#include <thread>

// POSIX system call
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>
#ifdef __APPLE__
#include <pthread.h>
#elif defined(__linux__)
#include <sys/syscall.h>
#include <unistd.h>
#endif

namespace corotracer {

// ==========================================
// 1. Memory layout (absolutely aligned with the Go side)
// ==========================================
struct alignas(64) Epoch {
    uint64_t timestamp;      // 8
    uint64_t tid;            // 8
    uint64_t addr;           // 8
    std::atomic<uint64_t> seq; // 8
    char reserved[31];       // 31
    bool is_active;          // 1
};                           // Total: 64 Bytes

struct alignas(1024) StationData {
    struct {
        uint64_t probe_id;   // 8
        uint64_t birth_ts;   // 8
        bool is_dead;        // 1
        char _pad[47];       // 47
    } header;                // 64 Bytes

    Epoch slots[8];          // 512 Bytes (8 * 64)

    // 🔴 Fix 1: Strictly pad to exactly 1024 bytes, reject compiler implicit padding
    // 64 + 512 + 448 = 1024 Bytes
    char flexible[448];
};

// 🔴 Fix 2: Expand GlobalHeader to 1024 bytes
struct alignas(1024) GlobalHeader {
    uint64_t magic_number;       // 8
    uint32_t version;            // 4
    uint32_t max_stations;       // 4
    std::atomic<uint32_t> allocated_count; // 4
    std::atomic<uint32_t> tracer_sleeping; // 4
    char _reserved[1000];        // 1024 - 24 = 1000 Bytes
};

// Global context
inline GlobalHeader* g_header = nullptr;
inline StationData* g_stations = nullptr;
inline int g_uds_fd = -1;

// Get the nanosecond-level timestamp
inline uint64_t get_ns() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return static_cast<uint64_t>(ts.tv_sec) * 1000000000ULL + ts.tv_nsec;
}

// Get the absolutely real operating system TID
inline uint64_t get_tid() {
#ifdef __APPLE__
    uint64_t tid;
    pthread_threadid_np(NULL, &tid);
    return tid;
#elif defined(__linux__)
    return static_cast<uint64_t>(::syscall(SYS_gettid));
#else
    return reinterpret_cast<uint64_t>(pthread_self());
#endif
}

// Wake up the Go-side engine at extreme speed
inline void trigger_uds_wakeup() {
    if (g_uds_fd != -1) {
        char wake_signal = '1';
        ::write(g_uds_fd, &wake_signal, 1);
    }
}

// ==========================================
// 2. Coroutine interceptor wrapper
// ==========================================
class PromiseMixin;

template <typename InnerAwaiter>
struct TracedAwaiter {
    InnerAwaiter inner;
    PromiseMixin* tracer;

    bool await_ready() { return inner.await_ready(); }

    template <typename Promise>
    auto await_suspend(std::coroutine_handle<Promise> h);

    auto await_resume();
};

// ==========================================
// 3. Mixin for user inheritance
// ==========================================
class PromiseMixin {
public:
    StationData* my_station = nullptr;
    uint64_t current_seq = 0;

    PromiseMixin() {
        if (!g_header) return;

        uint32_t max_stat = g_header->max_stations;
        uint32_t idx = g_header->allocated_count.fetch_add(1, std::memory_order_relaxed);

        if (idx < max_stat) {
            my_station = &g_stations[idx];
            my_station->header.probe_id = reinterpret_cast<uint64_t>(this);
            my_station->header.birth_ts = get_ns();
            my_station->header.is_dead = false;
        }
    }

    ~PromiseMixin() {
        if (my_station) {
            my_station->header.is_dead = true;
        }
    }

    template <typename Awaitable>
    auto await_transform(Awaitable&& awaitable) {
        return TracedAwaiter<Awaitable>{std::forward<Awaitable>(awaitable), this};
    }

    inline void write_trace(uint64_t addr, bool is_active) {
        if (!my_station) return;

        current_seq++;
        auto& slot = my_station->slots[current_seq % 8];

        slot.addr = addr;
        slot.tid = get_tid();
        slot.timestamp = get_ns();
        slot.is_active = is_active;

        slot.seq.store(current_seq, std::memory_order_release);

        if (g_header->tracer_sleeping.load(std::memory_order_relaxed) == 1) {
            trigger_uds_wakeup();
        }
    }
};

// ==========================================
// 2.1 Supplement the implementation of the interceptor
// ==========================================
template <typename InnerAwaiter>
template <typename Promise>
auto TracedAwaiter<InnerAwaiter>::await_suspend(std::coroutine_handle<Promise> h) {
    tracer->write_trace(reinterpret_cast<uint64_t>(h.address()), false);
    return inner.await_suspend(h);
}

template <typename InnerAwaiter>
auto TracedAwaiter<InnerAwaiter>::await_resume() {
    tracer->write_trace(0, true);
    return inner.await_resume();
}

// ==========================================
// 4. SDK initialization
// ==========================================
inline void InitTracer() {
    const char* shm_path = std::getenv("CTP_SHM_PATH");
    const char* sock_path = std::getenv("CTP_SOCK_PATH");
    const char* max_stations_str = std::getenv("CTP_MAX_STATIONS");

    if (!shm_path || !sock_path || !max_stations_str) {
        std::cerr << "[coroTracer] Env vars missing. Running in untraced mode." << std::endl;
        return;
    }

    int max_stations = std::atoi(max_stations_str);

    size_t mem_size = 1024 + (max_stations * 1024);

    int shm_fd = ::open(shm_path, O_RDWR);
    if (shm_fd < 0) {
        std::cerr << "[coroTracer] Failed to open shm: " << shm_path << std::endl;
        return;
    }

    void* mapped = ::mmap(nullptr, mem_size, PROT_READ | PROT_WRITE, MAP_SHARED, shm_fd, 0);
    if (mapped == MAP_FAILED) {
        std::cerr << "[coroTracer] Failed to mmap." << std::endl;
        ::close(shm_fd);
        return;
    }
    ::close(shm_fd);

    g_header = static_cast<GlobalHeader*>(mapped);

    g_stations = reinterpret_cast<StationData*>(static_cast<char*>(mapped) + 1024);

    g_uds_fd = ::socket(AF_UNIX, SOCK_STREAM, 0);
    if (g_uds_fd >= 0) {
        struct sockaddr_un addr;
        std::memset(&addr, 0, sizeof(addr));
        addr.sun_family = AF_UNIX;
        std::strncpy(addr.sun_path, sock_path, sizeof(addr.sun_path) - 1);

        if (::connect(g_uds_fd, (struct sockaddr*)&addr, sizeof(addr)) < 0) {
            std::cerr << "[coroTracer] Failed to connect UDS, sleep/wake may not work." << std::endl;
            ::close(g_uds_fd);
            g_uds_fd = -1;
        } else {
            int flags = ::fcntl(g_uds_fd, F_GETFL, 0);
            ::fcntl(g_uds_fd, F_SETFL, flags | O_NONBLOCK);
        }
    }

    std::cout << "[coroTracer] C++ SDK Successfully Attached!" << std::endl;
}

} // namespace corotracer