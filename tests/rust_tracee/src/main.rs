//! coroTracer integration-test tracee.
//!
//! When launched under `coroTracer -cmd`, the Go engine will capture every
//! suspend / resume event produced by `.traced()` futures.  When launched
//! standalone (no env vars), the SDK degrades to a no-op so all scenarios
//! still execute and verify Rust-side correctness.
//!
//! The binary prints `ALL_SCENARIOS_PASSED` on success; the shell script
//! greps for that string to confirm the run completed cleanly.

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use corotracer::{init_tracer, tracer_enabled, PollTrace, TraceFutureExt};
use tokio::sync::{mpsc, oneshot, Barrier};
use tokio::time::{sleep, Duration};

// ─── Entry point ──────────────────────────────────────────────────────────────

#[tokio::main(flavor = "multi_thread", worker_threads = 4)]
async fn main() {
    // Always call init_tracer first; if env vars are missing it becomes a no-op.
    init_tracer();

    if tracer_enabled() {
        println!("[rust_tracee] coroTracer attached successfully");
    } else {
        println!("[rust_tracee] Running in untraced mode (no CTP env vars)");
    }

    run_scenario(1, "single sleep",          scenario_single_sleep()).await;
    run_scenario(2, "many concurrent tasks", scenario_many_tasks(20)).await;
    run_scenario(3, "multiple suspensions",  scenario_multi_suspend(8)).await;
    run_scenario(4, "oneshot channel",       scenario_oneshot_channel()).await;
    run_scenario(5, "mpsc channel",          scenario_mpsc_channel(10)).await;
    run_scenario(6, "barrier rendezvous",    scenario_barrier(8)).await;
    run_scenario(7, "yield_now",             scenario_yield_now(6)).await;
    run_scenario(8, "mixed active / suspend",scenario_mixed_events()).await;
    run_scenario(9, "stress 100 tasks",      scenario_stress(100)).await;
    run_scenario(10,"nested future chain",   scenario_nested_chain(6)).await;
    run_scenario(11,"PollTrace low-level",   scenario_poll_trace_api()).await;
    run_scenario(12,"drop before ready",     scenario_drop_traced_future()).await;

    println!("[rust_tracee] ALL_SCENARIOS_PASSED");
}

async fn run_scenario<F>(id: u32, name: &str, fut: F)
where
    F: std::future::Future<Output = ()>,
{
    println!("[rust_tracee] --- Scenario {id}: {name} ---");
    fut.await;
    println!("[rust_tracee] [PASS] Scenario {id}: {name}");
}

// ─── Scenario implementations ─────────────────────────────────────────────────

/// Scenario 1 – a single future suspends once via sleep.
async fn scenario_single_sleep() {
    sleep(Duration::from_millis(1)).traced().await;
}

/// Scenario 2 – 20 concurrent tasks, each suspending once.
async fn scenario_many_tasks(n: usize) {
    let mut handles = Vec::with_capacity(n);
    for i in 0..n {
        handles.push(tokio::spawn(
            async move {
                sleep(Duration::from_millis(1 + (i % 3) as u64)).await;
                i * 2
            }
            .traced(),
        ));
    }
    for h in handles {
        h.await.expect("task panicked");
    }
}

/// Scenario 3 – one traced future suspends `count` times.
async fn scenario_multi_suspend(count: usize) {
    async move {
        for _ in 0..count {
            sleep(Duration::from_millis(1)).await;
        }
    }
    .traced()
    .await;
}

/// Scenario 4 – producer / consumer via oneshot channel.
async fn scenario_oneshot_channel() {
    let (tx, rx) = oneshot::channel::<u64>();

    let producer = tokio::spawn(
        async move {
            sleep(Duration::from_millis(2)).await;
            tx.send(42).expect("send failed");
        }
        .traced(),
    );

    let consumer = tokio::spawn(
        async move {
            let val = rx.await.expect("recv failed");
            assert_eq!(val, 42, "wrong value from oneshot");
        }
        .traced(),
    );

    producer.await.expect("producer panicked");
    consumer.await.expect("consumer panicked");
}

/// Scenario 5 – n producers write to an mpsc channel; one consumer reads all.
async fn scenario_mpsc_channel(n: usize) {
    let (tx, mut rx) = mpsc::channel::<usize>(n + 1);

    let mut producers = Vec::with_capacity(n);
    for i in 0..n {
        let tx = tx.clone();
        producers.push(tokio::spawn(
            async move {
                sleep(Duration::from_millis(1)).await;
                tx.send(i).await.expect("send failed");
            }
            .traced(),
        ));
    }
    // Drop original sender so the channel closes after all clones finish.
    drop(tx);

    let consumer = tokio::spawn(
        async move {
            let mut sum = 0usize;
            while let Some(v) = rx.recv().await {
                sum += v;
            }
            // 0+1+…+(n-1)
            sum
        }
        .traced(),
    );

    for h in producers {
        h.await.expect("producer panicked");
    }
    let sum = consumer.await.expect("consumer panicked");
    let expected: usize = (0..n).sum();
    assert_eq!(sum, expected, "mpsc sum mismatch");
}

/// Scenario 6 – a barrier forces all `n` tasks to reach the same point.
async fn scenario_barrier(n: usize) {
    let barrier = Arc::new(Barrier::new(n));
    let counter = Arc::new(AtomicU64::new(0));

    let mut handles = Vec::with_capacity(n);
    for _ in 0..n {
        let b = barrier.clone();
        let c = counter.clone();
        handles.push(tokio::spawn(
            async move {
                sleep(Duration::from_millis(1)).await;
                c.fetch_add(1, Ordering::Relaxed);
                b.wait().await;
            }
            .traced(),
        ));
    }

    for h in handles {
        h.await.expect("task panicked");
    }
    assert_eq!(counter.load(Ordering::Relaxed), n as u64);
}

/// Scenario 7 – explicit yield_now suspensions without any I/O.
async fn scenario_yield_now(count: usize) {
    async move {
        for _ in 0..count {
            tokio::task::yield_now().await;
        }
    }
    .traced()
    .await;
}

/// Scenario 8 – mix of active and suspend events; verifies both are emitted.
async fn scenario_mixed_events() {
    let counter = Arc::new(AtomicU64::new(0));
    let mut handles = Vec::with_capacity(8);

    for i in 0u64..8 {
        let c = counter.clone();
        handles.push(tokio::spawn(
            async move {
                // suspend
                sleep(Duration::from_millis(1 + i % 3)).await;
                // resume → do work → suspend again
                c.fetch_add(1, Ordering::Relaxed);
                tokio::task::yield_now().await;
                c.fetch_add(1, Ordering::Relaxed);
            }
            .traced(),
        ));
    }

    for h in handles {
        h.await.expect("task panicked");
    }
    assert_eq!(counter.load(Ordering::Relaxed), 16);
}

/// Scenario 9 – stress test with 100 concurrent traced tasks.
async fn scenario_stress(n: usize) {
    let mut handles = Vec::with_capacity(n);
    for i in 0..n {
        handles.push(tokio::spawn(
            async move {
                let ms = (i % 5 + 1) as u64;
                sleep(Duration::from_millis(ms)).await;
                tokio::task::yield_now().await;
                sleep(Duration::from_millis(1)).await;
                i
            }
            .traced(),
        ));
    }
    let mut completed = 0usize;
    for h in handles {
        h.await.expect("task panicked");
        completed += 1;
    }
    assert_eq!(completed, n);
}

/// Scenario 10 – a traced future drives a chain of inner awaits.
async fn scenario_nested_chain(depth: usize) {
    let result = async move {
        let mut acc = 0u64;
        for _ in 0..depth {
            sleep(Duration::from_millis(1)).await;
            acc += 1;
        }
        acc
    }
    .traced()
    .await;

    assert_eq!(result, depth as u64);
}

/// Scenario 11 – exercise the low-level `PollTrace` API directly, without
/// an async runtime driving it.  This verifies the probe is safe to use
/// even when the tracer is disabled.
async fn scenario_poll_trace_api() {
    // Allocate a probe; in untraced mode this is all no-ops.
    let mut probe = PollTrace::new();

    // is_enabled() must not panic either way.
    let _ = probe.is_enabled();

    // Lifecycle: suspend → resume → suspend → resume → dead
    probe.on_pending_addr(0xDEAD_BEEF_CAFE_0001);
    probe.on_resume();
    probe.on_pending_addr(0xDEAD_BEEF_CAFE_0002);
    probe.on_resume();
    probe.mark_dead();

    // mark_dead after mark_dead must be idempotent.
    probe.mark_dead();

    // Drop also calls mark_dead internally.
    drop(probe);

    // Verify a fresh probe that goes directly from new to drop.
    let _ephemeral = PollTrace::new();
    // Dropped here → mark_dead called by Drop impl.
}

/// Scenario 12 – a `TracedFuture` that is dropped before it resolves.
/// The Drop impl on `TracedFuture` (via `PollTrace`) must mark the station dead.
async fn scenario_drop_traced_future() {
    // Create a future that would sleep for a very long time …
    let long_sleep = sleep(Duration::from_secs(9999)).traced();

    // … but wrap it in a select! with a shorter timeout so it gets dropped.
    tokio::select! {
        _ = long_sleep => {
            panic!("long sleep should not have completed");
        }
        _ = sleep(Duration::from_millis(5)) => {
            // The long_sleep TracedFuture is dropped here.
        }
    }
}

// ─── Unit tests (cargo test, no Go engine needed) ─────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use corotracer::{tracer_enabled, PollTrace, TraceFutureExt};
    use std::future::Future;
    use std::pin::Pin;
    use std::sync::atomic::{AtomicUsize, Ordering as AtomicOrdering};
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    // In a unit-test environment there are no CTP env vars, so the SDK
    // should always initialise in Disabled mode.
    #[test]
    fn sdk_disabled_without_env_vars() {
        // init_tracer is idempotent; calling it again is safe.
        init_tracer();
        // tracer_enabled() may return true if a previous test (or the OS
        // environment) set the vars.  We can only assert it doesn't panic.
        let _ = tracer_enabled();
    }

    // PollTrace::new() must never panic regardless of tracer state.
    #[test]
    fn poll_trace_new_does_not_panic() {
        let _p = PollTrace::new();
    }

    // is_enabled() on a freshly allocated probe must not panic.
    #[test]
    fn poll_trace_is_enabled_does_not_panic() {
        let p = PollTrace::new();
        let _ = p.is_enabled();
    }

    // mark_dead must be idempotent.
    #[test]
    fn poll_trace_mark_dead_idempotent() {
        let mut p = PollTrace::new();
        p.mark_dead();
        p.mark_dead();
        p.mark_dead();
    }

    // on_pending_addr + on_resume cycle must not panic.
    #[test]
    fn poll_trace_suspend_resume_cycle() {
        let mut p = PollTrace::new();
        for addr in [0u64, 0xDEAD_BEEF, 0xFFFF_FFFF_FFFF_FFFF] {
            p.on_pending_addr(addr);
            p.on_resume();
        }
        p.mark_dead();
    }

    // on_resume without prior on_pending must be a no-op (not panic).
    #[test]
    fn poll_trace_resume_without_pending() {
        let mut p = PollTrace::new();
        p.on_resume();
        p.on_resume();
    }

    // Drop impl must mark dead without panicking.
    #[test]
    fn poll_trace_drop_calls_mark_dead() {
        let p = PollTrace::new();
        drop(p); // must not panic
    }

    // TracedFuture must preserve the inner future's output value.
    #[tokio::test]
    async fn traced_future_preserves_output() {
        let result = async { 42u64 }.traced().await;
        assert_eq!(result, 42);
    }

    // TracedFuture must preserve Poll::Pending semantics.
    #[test]
    fn traced_future_preserves_pending_semantics() {
        let polls = AtomicUsize::new(0);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = Box::pin(
            OnePendingThenReady { polls: &polls, value: 99u32 }.traced()
        );

        assert!(matches!(fut.as_mut().poll(&mut cx), Poll::Pending));
        assert!(matches!(fut.as_mut().poll(&mut cx), Poll::Ready(99u32)));
        assert_eq!(polls.load(AtomicOrdering::Relaxed), 2);
    }

    // TracedFuture<F: Send> must be Send (required by tokio multi-thread).
    #[test]
    fn traced_future_is_send() {
        fn assert_send<T: Send>() {}
        assert_send::<PollTrace>();
        assert_send::<corotracer::TracedFuture<std::future::Ready<()>>>();
    }

    // .traced() extension method must work on arbitrary futures.
    #[tokio::test]
    async fn trace_future_ext_method() {
        use tokio::time::{sleep, Duration};
        let v = async {
            sleep(Duration::from_millis(1)).await;
            7u32
        }
        .traced()
        .await;
        assert_eq!(v, 7);
    }

    // Multiple traced futures run concurrently on tokio multi-thread.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn multiple_traced_futures_concurrent() {
        use tokio::time::{sleep, Duration};
        let a = tokio::spawn(async { sleep(Duration::from_millis(1)).await; 1u32 }.traced());
        let b = tokio::spawn(async { sleep(Duration::from_millis(1)).await; 2u32 }.traced());
        let c = tokio::spawn(async { sleep(Duration::from_millis(1)).await; 3u32 }.traced());
        assert_eq!(a.await.unwrap(), 1);
        assert_eq!(b.await.unwrap(), 2);
        assert_eq!(c.await.unwrap(), 3);
    }

    // on_pending with a Pin<&mut F> reference must not panic.
    #[test]
    fn poll_trace_on_pending_with_pin() {
        let mut probe = PollTrace::new();
        let mut inner = std::future::ready(());
        let pinned = Pin::new(&mut inner);
        probe.on_pending(pinned);
        probe.on_resume();
        probe.mark_dead();
    }

    // A TracedFuture that is dropped mid-flight must not panic.
    #[test]
    fn traced_future_drop_mid_flight() {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let polls = AtomicUsize::new(0);
        let mut fut = Box::pin(
            OnePendingThenReady { polls: &polls, value: 0u32 }.traced()
        );
        // Poll once (returns Pending) then drop.
        let _ = fut.as_mut().poll(&mut cx);
        drop(fut);
    }

    // ── Test helpers ──────────────────────────────────────────────────────────

    struct OnePendingThenReady<'a, T: Copy> {
        polls: &'a AtomicUsize,
        value: T,
    }

    impl<T: Copy + Unpin> Future for OnePendingThenReady<'_, T> {
        type Output = T;
        fn poll(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<T> {
            let n = self.polls.fetch_add(1, AtomicOrdering::Relaxed);
            if n == 0 { Poll::Pending } else { Poll::Ready(self.value) }
        }
    }

    fn noop_waker() -> Waker {
        unsafe { Waker::from_raw(noop_raw_waker()) }
    }

    fn noop_raw_waker() -> RawWaker {
        RawWaker::new(std::ptr::null(), &NOOP_VTABLE)
    }

    unsafe fn noop_clone(_: *const ()) -> RawWaker { noop_raw_waker() }
    unsafe fn noop_wake(_: *const ()) {}
    unsafe fn noop_wake_ref(_: *const ()) {}
    unsafe fn noop_drop(_: *const ()) {}

    static NOOP_VTABLE: RawWakerVTable =
        RawWakerVTable::new(noop_clone, noop_wake, noop_wake_ref, noop_drop);
}
