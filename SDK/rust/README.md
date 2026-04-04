# coroTracer Rust SDK

This directory contains a framework-free Rust SDK that follows the same cTP
shared-memory contract as the C++20 SDK, but maps Rust's `Future::poll`
transitions into trace events.

It does not depend on Tokio, but `TracedFuture<F>` is `Send` whenever `F` is
`Send`, so it can be used with `tokio::spawn(...)` on the multi-thread runtime.

## Design mapping

- `init_tracer()` reads `CTP_SHM_PATH`, `CTP_SOCK_PATH`, and `CTP_MAX_STATIONS`
  and attaches to the already-created shared memory and UDS channel.
- `PollTrace::on_resume()` corresponds to the C++ `await_resume` active event.
- `PollTrace::on_pending()` / `on_pending_addr()` corresponds to the C++
  `await_suspend` inactive event.
- The write protocol is exactly the Lean/C++ discipline:
  odd `seq` first, payload second, even `seq` last, all with release ordering.

## Minimal usage

```rust
use corotracer::{init_tracer, TraceFutureExt};

async fn worker() {
    // ...
}

fn main() {
    init_tracer();

    let traced = worker().traced();
    // hand `traced` to your own executor / poll loop
}
```

## Runtime-author integration

If you already own the poll loop, use `PollTrace` directly:

```rust
use corotracer::PollTrace;
use std::pin::Pin;
use std::task::{Context, Poll};

fn poll_one<F>(
    mut future: Pin<&mut F>,
    cx: &mut Context<'_>,
    trace: &mut PollTrace,
) -> Poll<F::Output>
where
    F: std::future::Future,
{
    trace.on_resume();

    match future.as_mut().poll(cx) {
        Poll::Pending => {
            trace.on_pending(future.as_mut());
            Poll::Pending
        }
        Poll::Ready(output) => {
            trace.on_ready();
            Poll::Ready(output)
        }
    }
}
```

## Address semantics

Rust's stable poll model does not expose a direct suspension program counter like
C++ coroutines do. This SDK therefore uses the pinned future frame pointer as
the `addr` payload for `Poll::Pending`, which still satisfies the repository's
"suspension address or related coroutine address" requirement.
