#![forbid(unsafe_op_in_unsafe_fn)]

#[cfg(not(unix))]
compile_error!("corotracer Rust SDK currently supports Unix platforms only.");

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
compile_error!("corotracer Rust SDK currently supports macOS and Linux only.");

use std::env;
use std::fmt;
use std::fs::OpenOptions;
use std::future::Future;
use std::io;
use std::mem::{align_of, size_of};
use std::os::fd::{AsRawFd, IntoRawFd, RawFd};
use std::os::raw::c_int;
#[cfg(target_os = "linux")]
use std::os::raw::c_long;
use std::os::unix::net::UnixStream;
use std::pin::Pin;
use std::ptr;
use std::sync::atomic::{fence, AtomicU32, AtomicU64, Ordering};
use std::sync::OnceLock;
use std::task::{Context, Poll};
use std::time::Instant;

const MAGIC_NUMBER: u64 = 0x434F524F54524352;
const HEADER_SIZE: usize = 1024;
const STATION_SIZE: usize = 1024;
const UDS_WAKEUP_BYTE: u8 = b'1';
const DISCONNECTED_FD: RawFd = -1;
const SUSPEND_ADDR_NONE: u64 = 0;

const PROT_READ: c_int = 0x1;
const PROT_WRITE: c_int = 0x2;
const MAP_SHARED: c_int = 0x01;

#[cfg(target_os = "linux")]
const CLOCK_MONOTONIC: c_int = 1;
#[cfg(target_os = "macos")]
const CLOCK_MONOTONIC: c_int = 6;

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
const SYS_GETTID: c_long = 186;
#[cfg(all(target_os = "linux", target_arch = "aarch64"))]
const SYS_GETTID: c_long = 178;
#[cfg(all(
    target_os = "linux",
    not(any(target_arch = "x86_64", target_arch = "aarch64"))
))]
compile_error!("corotracer Rust SDK currently supports x86_64 and aarch64 on Linux.");

static TRACER_STATE: OnceLock<TracerState> = OnceLock::new();
static NEXT_PROBE_ID: AtomicU64 = AtomicU64::new(1);
static FALLBACK_CLOCK_BASE: OnceLock<Instant> = OnceLock::new();

#[repr(C, align(64))]
struct Epoch {
    timestamp: u64,
    tid: u64,
    addr: u64,
    seq: AtomicU64,
    reserved: [u8; 31],
    is_active: bool,
}

#[repr(C)]
struct StationHeader {
    probe_id: u64,
    birth_ts: u64,
    is_dead: bool,
    _pad: [u8; 47],
}

#[repr(C, align(1024))]
struct StationData {
    header: StationHeader,
    slots: [Epoch; 8],
    flexible: [u8; 448],
}

#[repr(C, align(1024))]
struct GlobalHeader {
    magic_number: u64,
    version: u32,
    max_stations: u32,
    allocated_count: AtomicU32,
    tracer_sleeping: AtomicU32,
    _reserved: [u8; 1000],
}

#[repr(C)]
struct Timespec {
    tv_sec: i64,
    tv_nsec: i64,
}

const _: [(); 64] = [(); size_of::<Epoch>()];
const _: [(); 64] = [(); align_of::<Epoch>()];
const _: [(); 1024] = [(); size_of::<StationData>()];
const _: [(); 1024] = [(); align_of::<StationData>()];
const _: [(); 1024] = [(); size_of::<GlobalHeader>()];
const _: [(); 1024] = [(); align_of::<GlobalHeader>()];

enum TracerState {
    Enabled(TracerRuntime),
    Disabled,
}

struct TracerRuntime {
    header: *mut GlobalHeader,
    stations: *mut StationData,
    uds_fd: RawFd,
    _mapping_ptr: *mut core::ffi::c_void,
    _mapping_len: usize,
}

unsafe impl Send for TracerRuntime {}
unsafe impl Sync for TracerRuntime {}

#[derive(Debug)]
enum InitError {
    MissingEnv,
    InvalidStationCount(String),
    Overflow,
    OpenShm(io::Error),
    Mmap(io::Error),
}

impl fmt::Display for InitError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingEnv => write!(f, "Env vars missing"),
            Self::InvalidStationCount(value) => {
                write!(f, "Invalid CTP_MAX_STATIONS value: {value}")
            }
            Self::Overflow => write!(f, "Shared memory size overflow"),
            Self::OpenShm(err) => write!(f, "Failed to open shm: {err}"),
            Self::Mmap(err) => write!(f, "Failed to mmap shared memory: {err}"),
        }
    }
}

/// Initializes the global coroTracer attachment state from
/// `CTP_SHM_PATH`, `CTP_SOCK_PATH`, and `CTP_MAX_STATIONS`.
///
/// This is idempotent. On failure the SDK degrades to a no-op mode, matching
/// the behavior of the C++ SDK.
pub fn init_tracer() {
    let _ = tracer_state();
}

/// Returns whether the current process successfully attached to coroTracer.
pub fn tracer_enabled() -> bool {
    matches!(tracer_state(), TracerState::Enabled(_))
}

/// A low-level per-future probe for runtimes that already own the poll loop.
///
/// `on_resume` maps to the C++ active event.
/// `on_pending` maps to the C++ suspend event.
/// `mark_dead` updates the station lifecycle bit.
pub struct PollTrace {
    runtime: Option<&'static TracerRuntime>,
    station: *mut StationData,
    event_count: u64,
    pending: bool,
    dead: bool,
}

// A traced future/task owns exactly one station and is only polled with `&mut self`.
// Moving that ownership across executor threads is sound, which is what Tokio's
// multi-thread scheduler needs. We intentionally do not implement `Sync`.
unsafe impl Send for PollTrace {}

impl PollTrace {
    /// Allocates a station for one Rust future/task.
    pub fn new() -> Self {
        let probe_id = NEXT_PROBE_ID.fetch_add(1, Ordering::Relaxed);
        let birth_ts = monotonic_ns();

        let runtime = match tracer_state() {
            TracerState::Enabled(runtime) => Some(runtime),
            TracerState::Disabled => None,
        };

        let Some(runtime) = runtime else {
            return Self::disabled();
        };

        let header = unsafe { &*runtime.header };
        let idx = header.allocated_count.fetch_add(1, Ordering::Relaxed);
        if idx >= header.max_stations {
            return Self::disabled();
        }

        let station = unsafe { runtime.stations.add(idx as usize) };
        unsafe {
            (*station).header.probe_id = probe_id;
            (*station).header.birth_ts = birth_ts;
            (*station).header.is_dead = false;
        }

        Self {
            runtime: Some(runtime),
            station,
            event_count: 0,
            pending: false,
            dead: false,
        }
    }

    /// Returns whether the probe is attached to a valid station.
    pub fn is_enabled(&self) -> bool {
        self.runtime.is_some() && !self.station.is_null()
    }

    /// Emits the "resume / active" transition with `addr = 0`.
    pub fn on_resume(&mut self) {
        if self.pending {
            self.write_trace(SUSPEND_ADDR_NONE, true);
            self.pending = false;
        }
    }

    /// Emits the "suspend / inactive" transition using the pinned future frame
    /// pointer as the trace address.
    pub fn on_pending<F>(&mut self, future: Pin<&mut F>) {
        let addr = future.as_ref().get_ref() as *const F as usize as u64;
        self.on_pending_addr(addr);
    }

    /// Emits the "suspend / inactive" transition with a caller-supplied
    /// address, for runtimes that already track a stable task frame pointer.
    pub fn on_pending_addr(&mut self, addr: u64) {
        self.write_trace(addr, false);
        self.pending = true;
    }

    /// Marks the traced future/task as dead.
    pub fn on_ready(&mut self) {
        self.mark_dead();
    }

    /// Marks the traced future/task as dead.
    pub fn mark_dead(&mut self) {
        if self.dead || self.station.is_null() {
            self.pending = false;
            self.dead = true;
            return;
        }

        unsafe {
            (*self.station).header.is_dead = true;
        }
        self.pending = false;
        self.dead = true;
    }

    fn disabled() -> Self {
        Self {
            runtime: None,
            station: ptr::null_mut(),
            event_count: 0,
            pending: false,
            dead: false,
        }
    }

    fn write_trace(&mut self, addr: u64, is_active: bool) {
        let Some(runtime) = self.runtime else {
            return;
        };
        if self.station.is_null() {
            return;
        }

        let slot_index = (self.event_count % 8) as usize;
        let slot = unsafe { &mut (*self.station).slots[slot_index] };

        let old_seq = slot.seq.load(Ordering::Relaxed);
        slot.seq.store(old_seq.wrapping_add(1), Ordering::Release);

        slot.addr = addr;
        slot.tid = get_tid();
        slot.timestamp = monotonic_ns();
        slot.is_active = is_active;

        slot.seq.store(old_seq.wrapping_add(2), Ordering::Release);
        self.event_count = self.event_count.wrapping_add(1);

        fence(Ordering::SeqCst);

        let header = unsafe { &*runtime.header };
        if header.tracer_sleeping.load(Ordering::Acquire) == 1
            && header
                .tracer_sleeping
                .compare_exchange(1, 0, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
        {
            trigger_uds_wakeup(runtime.uds_fd);
        }
    }
}

impl Default for PollTrace {
    fn default() -> Self {
        Self::new()
    }
}

impl Drop for PollTrace {
    fn drop(&mut self) {
        self.mark_dead();
    }
}

/// A framework-free wrapper that traces one Rust future using poll semantics.
#[must_use = "futures do nothing unless they are polled"]
pub struct TracedFuture<F> {
    inner: F,
    trace: PollTrace,
}

impl<F> TracedFuture<F> {
    pub fn new(inner: F) -> Self {
        Self {
            inner,
            trace: PollTrace::new(),
        }
    }

    pub fn into_inner(self) -> F {
        self.inner
    }

    pub fn trace_mut(&mut self) -> &mut PollTrace {
        &mut self.trace
    }
}

impl<F: Future> Future for TracedFuture<F> {
    type Output = F::Output;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = unsafe { self.get_unchecked_mut() };

        this.trace.on_resume();

        let result = unsafe { Pin::new_unchecked(&mut this.inner) }.poll(cx);

        match result {
            Poll::Pending => {
                let addr = (&this.inner as *const F) as usize as u64;
                this.trace.on_pending_addr(addr);
                Poll::Pending
            }
            Poll::Ready(output) => {
                this.trace.on_ready();
                Poll::Ready(output)
            }
        }
    }
}

/// Convenience constructor for wrapping one future.
pub fn trace_future<F: Future>(future: F) -> TracedFuture<F> {
    TracedFuture::new(future)
}

/// Extension trait so users can simply write `future.traced()`.
pub trait TraceFutureExt: Future + Sized {
    fn traced(self) -> TracedFuture<Self> {
        trace_future(self)
    }
}

impl<F: Future> TraceFutureExt for F {}

fn tracer_state() -> &'static TracerState {
    TRACER_STATE.get_or_init(|| match TracerRuntime::attach_from_env() {
        Ok(runtime) => {
            eprintln!("[coroTracer] Rust SDK Successfully Attached!");
            TracerState::Enabled(runtime)
        }
        Err(err) => {
            eprintln!("[coroTracer] {err}. Running in untraced mode.");
            TracerState::Disabled
        }
    })
}

impl TracerRuntime {
    fn attach_from_env() -> Result<Self, InitError> {
        let shm_path = env::var("CTP_SHM_PATH").map_err(|_| InitError::MissingEnv)?;
        let sock_path = env::var("CTP_SOCK_PATH").map_err(|_| InitError::MissingEnv)?;
        let max_stations = env::var("CTP_MAX_STATIONS").map_err(|_| InitError::MissingEnv)?;
        let max_stations = max_stations
            .parse::<usize>()
            .map_err(|_| InitError::InvalidStationCount(max_stations.clone()))?;

        let mem_size = HEADER_SIZE
            .checked_add(
                max_stations
                    .checked_mul(STATION_SIZE)
                    .ok_or(InitError::Overflow)?,
            )
            .ok_or(InitError::Overflow)?;

        let shm = OpenOptions::new()
            .read(true)
            .write(true)
            .open(&shm_path)
            .map_err(InitError::OpenShm)?;

        let mapped = unsafe {
            mmap(
                ptr::null_mut(),
                mem_size,
                PROT_READ | PROT_WRITE,
                MAP_SHARED,
                shm.as_raw_fd(),
                0,
            )
        };
        if mapped == map_failed() {
            return Err(InitError::Mmap(io::Error::last_os_error()));
        }

        let header = mapped as *mut GlobalHeader;
        let stations = unsafe { (mapped as *mut u8).add(HEADER_SIZE) as *mut StationData };

        unsafe {
            if (*header).magic_number != MAGIC_NUMBER {
                eprintln!(
                    "[coroTracer] Warning: unexpected magic number 0x{:016x}, continuing anyway.",
                    (*header).magic_number
                );
            }
        }

        let uds_fd = match UnixStream::connect(&sock_path) {
            Ok(stream) => {
                if let Err(err) = stream.set_nonblocking(true) {
                    eprintln!(
                        "[coroTracer] Failed to make UDS non-blocking, sleep/wake may not work: {err}"
                    );
                }
                stream.into_raw_fd()
            }
            Err(err) => {
                eprintln!("[coroTracer] Failed to connect UDS, sleep/wake may not work: {err}");
                DISCONNECTED_FD
            }
        };

        Ok(Self {
            header,
            stations,
            uds_fd,
            _mapping_ptr: mapped,
            _mapping_len: mem_size,
        })
    }
}

fn map_failed() -> *mut core::ffi::c_void {
    usize::MAX as *mut core::ffi::c_void
}

fn trigger_uds_wakeup(fd: RawFd) {
    if fd == DISCONNECTED_FD {
        return;
    }

    let wake = UDS_WAKEUP_BYTE;
    unsafe {
        let _ = write(fd, (&wake as *const u8).cast(), 1);
    }
}

fn monotonic_ns() -> u64 {
    let mut ts = Timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };

    let rc = unsafe { clock_gettime(CLOCK_MONOTONIC, &mut ts) };
    if rc == 0 {
        return (ts.tv_sec as u64)
            .saturating_mul(1_000_000_000)
            .saturating_add(ts.tv_nsec as u64);
    }

    FALLBACK_CLOCK_BASE
        .get_or_init(Instant::now)
        .elapsed()
        .as_nanos() as u64
}

#[cfg(target_os = "macos")]
fn get_tid() -> u64 {
    let mut tid = 0_u64;
    unsafe {
        let _ = pthread_threadid_np(ptr::null_mut(), &mut tid);
    }
    tid
}

#[cfg(target_os = "linux")]
fn get_tid() -> u64 {
    unsafe { syscall(SYS_GETTID) as u64 }
}

unsafe extern "C" {
    fn mmap(
        addr: *mut core::ffi::c_void,
        len: usize,
        prot: c_int,
        flags: c_int,
        fd: c_int,
        offset: i64,
    ) -> *mut core::ffi::c_void;
    fn write(fd: c_int, buf: *const core::ffi::c_void, count: usize) -> isize;
    fn clock_gettime(clock_id: c_int, tp: *mut Timespec) -> c_int;
    #[cfg(target_os = "macos")]
    fn pthread_threadid_np(thread: *mut core::ffi::c_void, thread_id: *mut u64) -> c_int;
    #[cfg(target_os = "linux")]
    fn syscall(number: c_long, ...) -> c_long;
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::future::Future;
    use std::pin::Pin;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::{RawWaker, RawWakerVTable, Waker};

    #[test]
    fn protocol_layout_matches_ctp_spec() {
        assert_eq!(size_of::<Epoch>(), 64);
        assert_eq!(align_of::<Epoch>(), 64);
        assert_eq!(size_of::<StationData>(), 1024);
        assert_eq!(align_of::<StationData>(), 1024);
        assert_eq!(size_of::<GlobalHeader>(), 1024);
        assert_eq!(align_of::<GlobalHeader>(), 1024);
    }

    #[test]
    fn traced_future_preserves_poll_semantics() {
        let polls = AtomicUsize::new(0);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut future = Box::pin(trace_future(OnePendingThenReady { polls: &polls }));

        assert!(matches!(future.as_mut().poll(&mut cx), Poll::Pending));
        assert!(matches!(future.as_mut().poll(&mut cx), Poll::Ready(7)));
        assert_eq!(polls.load(Ordering::Relaxed), 2);
    }

    #[test]
    fn traced_future_is_send_when_inner_future_is_send() {
        fn assert_send<T: Send>() {}

        assert_send::<PollTrace>();
        assert_send::<TracedFuture<std::future::Ready<()>>>();
    }

    struct OnePendingThenReady<'a> {
        polls: &'a AtomicUsize,
    }

    impl Future for OnePendingThenReady<'_> {
        type Output = usize;

        fn poll(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<Self::Output> {
            let next = self.polls.fetch_add(1, Ordering::Relaxed);
            if next == 0 {
                Poll::Pending
            } else {
                Poll::Ready(7)
            }
        }
    }

    fn noop_waker() -> Waker {
        unsafe { Waker::from_raw(noop_raw_waker()) }
    }

    fn noop_raw_waker() -> RawWaker {
        RawWaker::new(ptr::null(), &NOOP_WAKER_VTABLE)
    }

    unsafe fn noop_clone(_data: *const ()) -> RawWaker {
        noop_raw_waker()
    }

    unsafe fn noop(_data: *const ()) {}

    static NOOP_WAKER_VTABLE: RawWakerVTable =
        RawWakerVTable::new(noop_clone, noop, noop, noop);
}
