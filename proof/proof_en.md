# A Detailed Guide to coroTracer's Lean 4 Proof

This document explains what [`proof/proof.lean`](proof/proof.lean) proves, why the model is structured the way it is, and how each part maps back to the Go and C++ source code.

If you are reading this repository for the first time, it helps to keep the following files open side by side:

- `proof/proof.lean`: the formal proof itself
- `SDK/c++/coroTracer.h`: the C++ write side that records coroutine state into shared memory
- `structure/station.go`: the Go read side that scans shared memory lock-free
- `structure/jsonl.go`: the Go code that commits confirmed-safe records to JSONL
- `engine/engine.go`: the main Go engine loop, sleeping, and UDS wakeup
- `docs/cTP.md`: the protocol-level description

---

## 1. What the Lean proof actually proves

At a high level, the Lean proof focuses on two properties.

### 1.1 Safety: Go never writes half-written data into the log

More precisely, it shows that:

1. The Go side only commits a read when `seq` is even and unchanged across the two observations.
2. Therefore, a committed log entry cannot come from the middle of a C++ write.
3. In other words, the JSONL output cannot contain torn reads.

This corresponds to the core protocol in the real code:

- The C++ writer turns `seq` into an odd value before writing.
- The C++ writer turns `seq` into an even value after the payload is fully written.
- The Go reader first reads `seq`.
- If it sees an odd value, it skips the slot.
- If it sees an even value, it copies the payload into local variables.
- Then it reads `seq` again.
- If the two `seq` values differ, it discards the read.

### 1.2 Liveness: if C++ stops interfering briefly, Go can eventually harvest a record

More precisely, it proves an obstruction-free liveness result:

1. Assume a slot already contains a new, stable, even-`seq` value.
2. Assume Go is currently at the beginning of its scanning phase.
3. Assume C++ does not overwrite that slot during Go's "scan -> read payload -> validate" window.
4. Then Go will eventually commit that record into the log.

This is important to state carefully. The theorem does not say:

- "Go succeeds immediately under arbitrary concurrent interference."

It says:

- "If the writer leaves a short quiet window, the reader will complete successfully."

That is exactly the right obstruction-free guarantee for this kind of lock-free read protocol.

---

## 2. Why this proof matters

The heart of this project is not merely "can we read data?" but "is the data we read a complete, real, untorn snapshot of coroutine state?"

In a real concurrent system, the dangerous interleaving looks like this:

1. C++ is writing a new state into a shared-memory slot.
2. Go reads the same slot concurrently.
3. If Go reads the fields in the middle of the write, it may construct a fake record:
   - `tid` from the old version
   - `addr` from the new version
   - `timestamp` from a different moment
4. That fake record may not trip ASAN, TSAN, or any obvious memory-safety alarm, but the log is already semantically wrong.

This class of bug is especially nasty because:

- there may be no out-of-bounds access
- there may be no use-after-free
- there may not even be a detectable low-level memory corruption
- yet the trace is no longer trustworthy

The value of `proof/proof.lean` is that it turns "the log never contains that kind of mixed, half-written record" into a formal statement.

---

## 3. What the real protocol looks like

Before looking at Lean, it helps to restate the concrete protocol implemented by the code.

### 3.1 The C++ write side: odd first, payload next, even last

The key function is `PromiseMixin::write_trace` in `SDK/c++/coroTracer.h:149-182`.

At a high level, it behaves like this:

```cpp
slot = my_station->slots[event_count % 8]
old_seq = slot.seq.load(relaxed)

slot.seq.store(old_seq + 1, release)   // odd: write in progress

slot.addr = ...
slot.tid = ...
slot.timestamp = ...
slot.is_active = ...

slot.seq.store(old_seq + 2, release)   // even: write complete
event_count++
```

There are several important details here:

1. `event_count % 8` means each station contains an 8-slot ring buffer.
2. `old_seq + 1` turns `seq` into an odd value, which means "do not read this slot now."
3. The actual payload fields are written between the two `seq` stores.
4. `old_seq + 2` turns `seq` back into an even value, which means "this write transaction is now complete."
5. `event_count++` advances the ring position for the next event.

### 3.2 The Go read side: read `seq`, read payload, re-read `seq`

The key function is `(*StationData).Harvest` in `structure/station.go:42-87`.

The critical logic looks like this:

```go
seq1 := atomic.LoadUint64(&slot.Seq)
if seq1 <= lastSeen[i] { continue }
if seq1 % 2 != 0 { continue }

localTID := slot.TID
localAddr := slot.Addr
localIsActive := slot.IsActive
localTS := slot.Timestamp

seq2 := atomic.LoadUint64(&slot.Seq)
if seq1 != seq2 { continue }

sw.WriteSafeSlot(...)
lastSeen[i] = seq1
```

This naturally splits into three phases:

1. `ScanSeq`: check whether the slot is new and stable.
2. `ReadPayload`: copy fields from shared memory into local variables.
3. `ValidateSeq`: read `seq` again and verify the payload copy was not interrupted.

The `GoPC` states in Lean are a direct abstraction of these three phases.

### 3.3 The commit point: a record is only written after validation succeeds

The actual logging path is in `structure/jsonl.go:73-76`:

```go
sw.line = s.marshalSafeSlotJSONL(...)
_, err := sw.writer.Write(sw.line)
```

This only happens in the branch where `seq1 == seq2` after the second read.  
So if we can prove that every path reaching this point is safe, then the JSONL log is trustworthy.

### 3.4 The engine loop: repeatedly calling `Harvest`

`engine/engine.go` is mostly the surrounding runtime machinery:

- `NewTracerEngine` builds the shared memory region and the UDS listener.
- `doScan` loops over allocated stations and calls `Harvest`.
- `hotHarvestLoop` switches between busy scanning and sleeping until wakeup.

The Lean proof does not directly model UDS, sockets, mmap, or process launch.  
It focuses on the narrower and more fundamental question:

- Is the slot-level concurrent read/write protocol itself correct?

---

## 4. How the Lean model abstracts the real system

The following table is the most important mental bridge between the proof and the implementation.

| Lean object | Meaning | Source mapping | Why this abstraction works |
| --- | --- | --- | --- |
| `Slot.payload` | The payload of one slot | In reality this corresponds to `tid`, `addr`, `is_active`, `timestamp` | Lean compresses multiple fields into a single abstract payload because the proof only needs version consistency |
| `Slot.seq` | The sequence number of the slot | `Epoch.Seq` / `Epoch::seq` | This is the actual concurrency barrier |
| `CppPC.Idle` | C++ is not currently writing the slot | Before or after the write transaction in `write_trace` | The proof uses a program-counter-style writer state |
| `CppPC.WritingPayload` | C++ has marked the slot odd and is writing the payload | Between the two `slot.seq.store(...)` calls | This is the dangerous interval the Go side must avoid |
| `GoPC.ScanSeq` | Go is performing the first `seq` observation | `seq1 := atomic.LoadUint64(...)` plus the checks | Reader phase 1 |
| `GoPC.ReadPayload` | Go decided the slot is worth attempting to read | Copying `TID`, `Addr`, `IsActive`, `Timestamp` | Reader phase 2 |
| `GoPC.ValidateSeq` | Go copied the payload and is about to re-check `seq` | `seq2 := atomic.LoadUint64(...)` | Reader phase 3 |
| `cpp_idx` | Which ring slot the writer will use next | `event_count % 8` | Lean models ring progress explicitly |
| `go_last_seen` | The most recently committed `seq` for a slot | `TracerEngine.lastSeen [][8]uint64` | Used to filter old or duplicate records |
| `jsonl_log` | The set of committed safe records | Lines emitted by `WriteSafeSlot` | Lean cares about semantic commit, not file formatting |

### 4.1 Why Lean compresses the real payload into a single `Nat`

In the real implementation, one record includes multiple fields:

- `tid`
- `addr`
- `timestamp`
- `is_active`

Lean does not model each field separately. Instead, it combines them into:

```lean
payload : Nat
```

That is not a shortcut in the bad sense. It is the right abstraction for this proof.

The proof is not trying to show:

- "`tid` is safe independently"
- "`addr` is safe independently"

It is trying to show:

- "the payload committed by Go comes from a single stable version of the slot."

Once that is true for an abstract payload, it naturally transfers to the real grouped payload made of multiple fields.

### 4.2 Why Lean does not directly model stations, headers, or sockets

The proof is deliberately focused on the slot-level concurrency protocol.  
It does not formalize:

- the ABI layout of `GlobalHeader`
- the station allocation logic via `AllocatedCount`
- `TracerSleeping` and UDS wakeup
- the `InitTracer()` mmap and UDS connection flow
- process launch and environment injection in `main.go`

Those are all important engineering concerns, but they are not the core of the torn-read safety question.  
The Lean proof isolates the deepest correctness kernel:

- If C++ follows this `seq` protocol when writing, and Go follows this `seq` protocol when reading, can the log contain half-written data?

---

## 5. The structure of the Lean file

`proof/proof.lean` can be read as four major sections.

### 5.1 Section one: the state space

Location: `proof/proof.lean:8-33`

This section defines:

- `Slot`
- `CppPC`
- `GoPC`
- `SystemState`

This is the full state universe in which the proof operates.

### 5.2 Section two: the transition relation

Location: `proof/proof.lean:38-97`

This section defines:

```lean
inductive Step : SystemState -> SystemState -> Prop
```

That means:

- which atomic steps are allowed
- how each step transforms the system state

This is the core of the proof.  
Everything else is built on top of these state transitions.

### 5.3 Section three: the safety proof

Location: `proof/proof.lean:104-191`

This section first defines a system invariant `SystemInvariant`, then proves that:

- if the invariant holds before one step
- it still holds after that step

### 5.4 Section four: the liveness proof

Location: `proof/proof.lean:197-250`

This section proves that:

- if a new stable value is already present
- and Go performs its three reader phases
- and C++ stays quiet during those phases
- then the record will be committed

---

## 6. Explaining each `Step` constructor

This is the most important part to understand. Once the `Step` relation is clear, the theorems become much easier to read.

### 6.1 `cpp_start_write`

Location: `proof/proof.lean:41-46`

Lean definition:

```lean
| cpp_start_write (s : SystemState) :
    s.cpp_pc = CppPC.Idle ->
    Step s { s with
      cpp_pc := CppPC.WritingPayload,
      slots := fun i =>
        if i = s.cpp_idx then
          { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 }
        else s.slots i
    }
```

Meaning:

1. C++ may only start writing when it is currently idle.
2. Once writing begins, the writer program counter becomes `WritingPayload`.
3. The current slot's `seq` is incremented by one.

Since a stable version should be even, this step makes `seq` odd.  
An odd `seq` means:

- this slot is being written
- the reader must not trust it

Source mapping:

- `SDK/c++/coroTracer.h:157-159`

```cpp
uint64_t old_seq = slot.seq.load(std::memory_order_relaxed);
slot.seq.store(old_seq + 1, std::memory_order_release);
```

Why this abstraction is right:

- Lean does not care how `old_seq` was computed
- it only cares that the write transaction begins by switching the slot into the odd state

That is exactly what the Go side relies on to skip unstable data.

### 6.2 `cpp_write_data`

Location: `proof/proof.lean:49-53`

Lean definition:

```lean
| cpp_write_data (s : SystemState) (new_data : Nat) :
    s.cpp_pc = CppPC.WritingPayload ->
    Step s { s with
      slots := fun i =>
        if i = s.cpp_idx then
          { payload := new_data, seq := (s.slots i).seq }
        else s.slots i
    }
```

Meaning:

1. Payload writes can only happen while C++ is in `WritingPayload`.
2. This step changes the payload but does not change `seq`.
3. Therefore, throughout the middle of the write transaction, `seq` stays odd.

Source mapping:

- `SDK/c++/coroTracer.h:164-167`

```cpp
slot.addr = addr;
slot.tid = get_tid();
slot.timestamp = get_ns();
slot.is_active = is_active;
```

Lean makes an important abstraction here:

- the real code writes several fields
- the proof treats them as one logical payload update

That is fine because the proof target is not the business meaning of each field.  
The target is whether Go might commit data from the middle of the write interval.

### 6.3 `cpp_end_write`

Location: `proof/proof.lean:56-62`

Lean definition:

```lean
| cpp_end_write (s : SystemState) :
    s.cpp_pc = CppPC.WritingPayload ->
    Step s { s with
      cpp_pc := CppPC.Idle,
      slots := fun i =>
        if i = s.cpp_idx then
          { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 }
        else s.slots i,
      cpp_idx := (s.cpp_idx + 1) % 8
    }
```

Meaning:

1. Once the payload write is complete, C++ leaves `WritingPayload`.
2. The current slot's `seq` is incremented again, turning it from odd back to even.
3. The writer advances to the next ring slot.

Source mapping:

- `SDK/c++/coroTracer.h:169-174`

```cpp
slot.seq.store(old_seq + 2, std::memory_order_release);
event_count++;
```

Strictly speaking, the implementation advances `event_count`, not an explicit `cpp_idx`.  
But because the slot is chosen by `event_count % 8`, the Lean update

```lean
cpp_idx := (cpp_idx + 1) % 8
```

captures the same ring-advance behavior.

### 6.4 `go_scan`

Location: `proof/proof.lean:65-72`

Lean definition:

```lean
| go_scan (s : SystemState) :
    s.go_pc = GoPC.ScanSeq ->
    Step s { s with
      go_pc := if (s.slots s.go_scan_idx).seq % 2 = 0 /\
                  (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx
               then GoPC.ReadPayload
               else GoPC.ScanSeq,
      go_observed_seq := (s.slots s.go_scan_idx).seq
    }
```

This is the first phase of the Go reader:

1. Read the current slot's `seq`.
2. Store that observation into `go_observed_seq`.
3. If it is even and newer than `last_seen`, move into `ReadPayload`.
4. Otherwise remain in `ScanSeq`.

Source mapping:

- `structure/station.go:50-58`

```go
seq1 := atomic.LoadUint64(&slot.Seq)
if seq1 <= lastSeenSeqs[i] {
    continue
}
if seq1%2 != 0 {
    continue
}
```

Why Lean represents this as one explicit state transition while Go writes it as a handful of lines:

- Lean needs the reader phase structure to be visible in the state machine
- later proofs reason by case analysis on `GoPC`

### 6.5 `go_read`

Location: `proof/proof.lean:75-80`

Lean definition:

```lean
| go_read (s : SystemState) :
    s.go_pc = GoPC.ReadPayload ->
    Step s { s with
      go_pc := GoPC.ValidateSeq,
      go_temp_payload := (s.slots s.go_scan_idx).payload
    }
```

Meaning:

1. The reader has already decided this slot is worth trying.
2. It now copies the payload into a local temporary.
3. After the copy, it transitions into `ValidateSeq`.

Source mapping:

- `structure/station.go:64-67`

```go
localTID := slot.TID
localAddr := slot.Addr
localIsActive := slot.IsActive
localTS := slot.Timestamp
```

Lean's `go_temp_payload` is the abstraction of those local variables.

### 6.6 `go_validate_pass`

Location: `proof/proof.lean:83-89`

Lean definition:

```lean
| go_validate_pass (s : SystemState) :
    s.go_pc = GoPC.ValidateSeq /\
    (s.slots s.go_scan_idx).seq = s.go_observed_seq ->
    Step s { s with
      go_pc := GoPC.ScanSeq,
      go_last_seen := fun i =>
        if i = s.go_scan_idx then s.go_observed_seq else s.go_last_seen i,
      jsonl_log := (s.go_observed_seq, s.go_temp_payload) :: s.jsonl_log
    }
```

Meaning:

1. Go has already copied the payload and is now validating.
2. If the current slot `seq` still matches the earlier `go_observed_seq`,
3. then C++ did not disturb that slot during the payload read.
4. Therefore the result is safe to commit:
   - update `last_seen`
   - append the record to the log
   - return to `ScanSeq`

Source mapping:

- `structure/station.go:71-84`
- `structure/jsonl.go:73-76`

```go
seq2 := atomic.LoadUint64(&slot.Seq)
if seq1 != seq2 {
    continue
}

sw.WriteSafeSlot(s, seq1, localTID, localAddr, localIsActive, localTS)
lastSeenSeqs[i] = seq1
```

The crucial idea is:

- commit happens only after the second `seq` check succeeds

That is the whole safety hinge of the protocol.

### 6.7 `go_validate_fail`

Location: `proof/proof.lean:92-97`

Lean definition:

```lean
| go_validate_fail (s : SystemState) :
    s.go_pc = GoPC.ValidateSeq /\
    (s.slots s.go_scan_idx).seq != s.go_observed_seq ->
    Step s { s with
      go_pc := GoPC.ScanSeq
    }
```

Meaning:

1. Go copied the payload into a local temporary.
2. During validation, it discovers that the slot `seq` changed.
3. That means the read may have overlapped with another write or wrap-around.
4. The temporary payload must be discarded and must not be committed.

Source mapping:

- `structure/station.go:71-76`

```go
seq2 := atomic.LoadUint64(&slot.Seq)
if seq1 != seq2 {
    continue
}
```

That `continue` matters a lot.  
It means:

- do not update `lastSeen`
- do not write to JSONL
- simply retry later

Lean models exactly that behavior by resetting `go_pc` to `ScanSeq` and committing nothing.

---

## 7. What `SystemInvariant` means

Location: `proof/proof.lean:106-110`

The definition is:

```lean
def SystemInvariant (s : SystemState) : Prop :=
  (forall seq pay, (seq, pay) in s.jsonl_log -> seq % 2 = 0) /\
  (s.go_pc = GoPC.ReadPayload \/ s.go_pc = GoPC.ValidateSeq ->
    s.go_observed_seq % 2 = 0)
```

It has two parts.

### 7.1 Condition A: every `seq` already present in the log is even

This says:

- any record that has already been committed
- must correspond to a completed, stable version
- not to an odd `seq` write-in-progress state

This is the externally visible safety guarantee.

### 7.2 Condition B: if Go is in `ReadPayload` or `ValidateSeq`, then its observed `seq` is even

This says:

- Go never carries an odd `seq` into the actual payload-read path
- so it never proceeds based on a snapshot that was already marked unstable

This is the internal discipline that supports the external safety guarantee:

- log safety is the final property
- "observed `seq` is even" is one of the crucial internal facts needed to maintain that property

---

## 8. The first theorem: `system_is_always_safe`

Location: `proof/proof.lean:113-191`

The theorem statement is:

```lean
theorem system_is_always_safe (s1 s2 : SystemState) (h_step : Step s1 s2) :
    SystemInvariant s1 -> SystemInvariant s2 := by
```

This means:

- if the old state `s1` satisfies the invariant
- and `s1 -> s2` is one legal `Step`
- then the new state `s2` also satisfies the invariant

This is a one-step preservation theorem.  
It is not yet the full global statement "all reachable states are safe," but it is the crucial inductive building block for that larger result.

If we also prove:

1. the initial state satisfies `SystemInvariant`
2. multi-step executions are built out of repeated `Step`s

then we can derive the expected global safety story by induction.

### 8.1 The overall structure of the proof

The proof splits into two goals:

1. every `seq` in the new log is still even
2. if the new state is in `ReadPayload` or `ValidateSeq`, the new observed `seq` is still even

That is why the proof begins with:

```lean
constructor
```

It proves both halves of the invariant separately.

### 8.2 Goal A: why "all logged `seq`s are even" remains true

Location: `proof/proof.lean:122-147`

The proof splits into two major categories.

#### Case 1: the step does not change the log at all

For these constructors:

- `cpp_start_write`
- `cpp_write_data`
- `cpp_end_write`
- `go_scan`
- `go_read`
- `go_validate_fail`

Lean simply reuses the old invariant:

```lean
exact h_log_even seq pay h_in_s2
```

The reasoning is straightforward:

- because `jsonl_log` did not change
- any record in the new log already existed in the old log
- and the old invariant already says its `seq` was even

This matches the implementation exactly:

- the C++ writer never writes JSONL directly
- Go's scan phase does not write JSONL
- Go's read phase does not write JSONL
- validation failure explicitly discards the read

#### Case 2: the only step that can extend the log is `go_validate_pass`

This is the interesting branch in `proof/proof.lean:132-147`.

Lean expands membership in the new list:

```lean
simp only [List.mem_cons] at h_in_s2
cases h_in_s2 with
| inl h_eq => ...
| inr h_mem => ...
```

Meaning:

- either the record is the newly inserted head element
- or it was already in the old log

##### Subcase 2.1: the record is the newly inserted one

Lean uses:

```lean
injection h_eq with h_seq h_pay
rw [h_seq]
```

to identify the new log record's `seq` with `s.go_observed_seq`.

Then it calls the old invariant's second half:

```lean
apply h_obs_even
right
exact h_cond.left
```

The logical chain is:

1. `go_validate_pass` requires that the old state is in `GoPC.ValidateSeq`
2. the invariant says that whenever the old state is in `ReadPayload` or `ValidateSeq`, `go_observed_seq` must be even
3. therefore the newly inserted record also has an even `seq`

This is the key idea:

- the safety of a newly committed record is not accidental
- it is inherited from the fact that the reader was only ever allowed to enter the read/validate path with an even `seq`

##### Subcase 2.2: the record is an older log entry

Then Lean just goes back to the old invariant:

```lean
exact h_log_even seq pay h_mem
```

That part is immediate.

### 8.3 Goal B: why "if we are reading or validating, the observed `seq` is even" remains true

Location: `proof/proof.lean:152-191`

This part again proceeds by case analysis on `Step`.

#### Branch 1: the three C++ steps

Lean writes:

```lean
case cpp_start_write => apply h_obs_even; exact h_pc_s2
case cpp_write_data => apply h_obs_even; exact h_pc_s2
case cpp_end_write => apply h_obs_even; exact h_pc_s2
```

Why this works:

- these steps only affect the writer side
- they do not change `go_pc` or `go_observed_seq`
- so if the new state is in `ReadPayload` or `ValidateSeq`, the old state already was too
- and the old invariant can be reused directly

#### Branch 2: `go_scan`

This is the most interesting part, in `proof/proof.lean:160-172`.

Lean expands the `if`:

```lean
dsimp at h_pc_s2 ⊢
split at h_pc_s2
```

and considers the two branches.

##### Case 2.1: the `if` condition is true

That means:

- the observed `seq` is even
- and the observed `seq` is newer than `last_seen`

In that branch the new state enters `ReadPayload`, and Lean directly extracts:

```lean
exact h_if.left
```

which is exactly the proof that the observed `seq` is even.

This corresponds perfectly to the Go code:

```go
if seq1 <= lastSeen { continue }
if seq1%2 != 0 { continue }
```

Go only advances toward payload reading if both checks pass.

##### Case 2.2: the `if` condition is false

In that branch the new state remains in `ScanSeq`.  
But the goal hypothesis `h_pc_s2` says the new state is in `ReadPayload` or `ValidateSeq`.  
That is impossible, so Lean closes the branch by contradiction.

#### Branch 3: `go_read`

Location: `proof/proof.lean:175-178`

Here the reasoning is:

1. `go_read` moves from `ReadPayload` to `ValidateSeq`
2. so if the new state is in `ValidateSeq`, the old state was in `ReadPayload`
3. the old invariant already says the observed `seq` was even in that situation
4. the observed `seq` is carried over unchanged
5. therefore the new state also satisfies the property

Lean writes:

```lean
apply h_obs_even
left
exact h_pc
```

#### Branch 4: `go_validate_pass` and `go_validate_fail`

Both of these return the reader to `ScanSeq`.  
So if the goal hypothesis says the new state is in `ReadPayload` or `ValidateSeq`, that hypothesis is impossible.

Lean handles that with contradiction after expanding the state update.

### 8.4 What this theorem means for the real implementation

This theorem is not trying to prove:

- "Go always reads every record immediately"

It is proving:

- any record Go actually commits must satisfy the safety discipline

In other words, the system is explicitly designed to prefer:

- dropping a doubtful read and trying again later

over:

- committing a possibly torn snapshot

That is exactly what the real `Harvest` function does:

- skip odd `seq`
- skip mismatched `seq1` and `seq2`
- commit only when validation succeeds

This is a classic "consistency over immediate completeness" lock-free read pattern.

---

## 9. The second theorem: `go_obstruction_free_liveness`

Location: `proof/proof.lean:197-250`

In simplified prose, the theorem says:

> If Go starts in `ScanSeq`, the current slot already contains a new even `seq`, and C++ stays quiet throughout the three-step reader window, then Go will return to `ScanSeq` and the observed record will be present in the log.

### 9.1 Why this is called obstruction-free

Because the theorem is not claiming:

- "Go succeeds regardless of how aggressively C++ keeps overwriting the slot."

It claims:

- "Once interference goes away, the reader path completes."

That is the correct obstruction-free interpretation:

- progress is guaranteed in the absence of obstruction

This matches reality well. Under heavy contention, one particular read attempt may fail because `seq1 != seq2`. But as soon as there is a small quiet window, the reader can succeed.

### 9.2 Explaining the assumptions one by one

Location: `proof/proof.lean:198-209`

#### `h_go_pc`

```lean
(h_go_pc : s0.go_pc = GoPC.ScanSeq)
```

This says Go starts at the beginning of its scan phase.

That matches the start of the `Harvest` logic for a slot.

#### `h_new_data`

```lean
(h_new_data : (s0.slots s0.go_scan_idx).seq > s0.go_last_seen s0.go_scan_idx)
```

This says the slot actually contains something new rather than an already-consumed version.

That matches the Go condition:

```go
if seq1 <= lastSeenSeqs[i] {
    continue
}
```

Without this assumption, Go has no reason to proceed into payload reading.

#### `h_even`

```lean
(h_even : (s0.slots s0.go_scan_idx).seq % 2 = 0)
```

This says the data is already stable, not in the middle of a write.

That matches:

```go
if seq1%2 != 0 {
    continue
}
```

#### `h_quiet0` through `h_quiet3`

```lean
(h_quiet0 : s0.cpp_pc = CppPC.Idle)
(h_quiet1 : s1.cpp_pc = CppPC.Idle)
(h_quiet2 : s2.cpp_pc = CppPC.Idle)
(h_quiet3 : s3.cpp_pc = CppPC.Idle)
```

These say that C++ remains quiet throughout the three-step reader window.

In practice this means:

- no new overlapping write begins on the slot while the reader performs its scan, payload copy, and validation steps

One subtle note:

- the current proof body mainly uses `h_quiet0`, `h_quiet1`, and `h_quiet2`
- `h_quiet3` is not the key driver of the argument
- but keeping it in the theorem statement is still reasonable because it captures the intended "the whole window is quiet" story

### 9.3 Why three steps are necessary

Because a successful read in this protocol really is a three-phase path:

1. `go_scan`
2. `go_read`
3. `go_validate_pass`

That is the abstract form of the real `Harvest` logic.

### 9.4 How the proof proceeds

#### Step one: `step1` must be `go_scan`

Location: `proof/proof.lean:213-223`

Lean starts with:

```lean
cases step1
```

and eliminates all impossible branches:

- `cpp_start_write` contradicts `h_quiet0`
- `cpp_write_data` contradicts `h_quiet0`
- `cpp_end_write` contradicts `h_quiet0`
- `go_read` contradicts `h_go_pc = ScanSeq`
- `go_validate_pass` contradicts `h_go_pc = ScanSeq`
- `go_validate_fail` contradicts `h_go_pc = ScanSeq`

So the only possible first step is:

```lean
case go_scan h_pc => ...
```

Then Lean packages the assumptions:

```lean
have h_if : seq_even /\ seq_new := <...>
```

and concludes that the `if` in `go_scan` must take the true branch:

```lean
have h_s1_pc_is_read : ... = GoPC.ReadPayload := by simp [h_if]
```

So after the first step, Go must be in `ReadPayload`.

#### Step two: `step2` must be `go_read`

Location: `proof/proof.lean:225-234`

Lean again performs case analysis and rules out everything else:

- all C++ steps contradict `h_quiet1`
- `go_scan` contradicts the fact that the current state is already `ReadPayload`
- `go_validate_pass` and `go_validate_fail` also do not match the current program counter

So only:

```lean
case go_read h2 =>
```

remains.

That means Go has completed the payload copy and entered `ValidateSeq`.

#### Step three: `step3` must be `go_validate_pass`

Location: `proof/proof.lean:236-250`

Again Lean eliminates impossible branches:

- any C++ step contradicts `h_quiet2`
- `go_scan` and `go_read` do not match the current state

That leaves two apparent candidates:

- `go_validate_fail`
- `go_validate_pass`

Lean then excludes `go_validate_fail`:

```lean
case go_validate_fail h =>
  have hp := h.right
  exact False.elim (hp rfl)
```

The idea is:

- `go_validate_fail` requires the current slot `seq` to differ from `go_observed_seq`
- but during this quiet window C++ never touched the slot
- so the slot `seq` must still equal the previously observed one
- therefore the inequality is impossible

So only:

```lean
case go_validate_pass _ =>
  constructor
  · rfl
  · simp [List.mem_cons]
```

can happen.

That yields the final conclusion:

1. `s3.go_pc = GoPC.ScanSeq`
2. `(s3.go_observed_seq, s3.go_temp_payload) ∈ s3.jsonl_log`

which is exactly "Go successfully committed the record."

### 9.5 What this means for the real code

This theorem tells us that the Go reader is not succeeding by luck.  
If the writer leaves a sufficiently quiet window, the three-phase read protocol must succeed.

That corresponds directly to the concrete `Harvest` path:

1. `seq1` is new and even
2. fields are copied
3. `seq2` still equals `seq1`
4. `WriteSafeSlot` is executed

---

## 10. A compact Lean-to-source mapping table

This table is useful when cross-reading the proof and the implementation.

| Lean location | Meaning | Go/C++ source mapping |
| --- | --- | --- |
| `proof/proof.lean:41-46` | begin write: `seq` becomes odd | `SDK/c++/coroTracer.h:157-159` |
| `proof/proof.lean:49-53` | write payload | `SDK/c++/coroTracer.h:164-167` |
| `proof/proof.lean:56-62` | finish write: `seq` becomes even and ring advances | `SDK/c++/coroTracer.h:169-174` |
| `proof/proof.lean:65-72` | Go scans `seq` and decides whether to proceed | `structure/station.go:50-58` |
| `proof/proof.lean:75-80` | Go copies payload into local variables | `structure/station.go:64-67` |
| `proof/proof.lean:83-89` | Go validates successfully and commits | `structure/station.go:71-84` plus `structure/jsonl.go:73-76` |
| `proof/proof.lean:92-97` | Go validation fails and discards the read | `structure/station.go:71-76` |
| `proof/proof.lean:106-110` | system invariant | expressed across the control flow in `Harvest` |
| `proof/proof.lean:113-191` | safety preservation proof | explains why `Harvest` cannot commit torn data |
| `proof/proof.lean:197-250` | quiet-window liveness proof | explains why `Harvest` must succeed when not disturbed |

---

## 11. How one real event flows from C++ into the Go log

This section connects the proof back to the full runtime story.

### 11.1 A coroutine suspension triggers a C++ trace write

In `SDK/c++/coroTracer.h:191-193`:

```cpp
tracer->write_trace(reinterpret_cast<uint64_t>(h.address()), false);
```

This means:

- the coroutine is about to suspend
- the suspension state is written into the current station slot

Resumption is recorded in `SDK/c++/coroTracer.h:197-199`:

```cpp
tracer->write_trace(0, true);
```

which means:

- the coroutine became active again

### 11.2 C++ completes one shared-memory write transaction

Inside `write_trace`, the implementation:

1. selects `event_count % 8`
2. stores odd `seq`
3. writes the payload fields
4. stores even `seq`
5. increments `event_count`

In Lean, that is abstracted as:

1. `cpp_start_write`
2. `cpp_write_data`
3. `cpp_end_write`

### 11.3 If Go is sleeping, C++ tries to wake it up

In `SDK/c++/coroTracer.h:176-180`:

```cpp
if (g_header->tracer_sleeping.load(std::memory_order_acquire) == 1) {
    uint32_t expected = 1;
    if (g_header->tracer_sleeping.compare_exchange_strong(expected, 0, std::memory_order_acq_rel)) {
        trigger_uds_wakeup();
    }
}
```

This is part of the engineering-level wakeup path.  
It is important for performance and responsiveness, but it is not what the current Lean proof formalizes.

### 11.4 The Go engine scans stations

In `engine/engine.go:112-123`:

```go
allocated := atomic.LoadUint32(&e.header.AllocatedCount)
for i := uint32(0); i < allocated; i++ {
    totalHarvested += e.stations[i].Harvest(&e.lastSeen[i], e.writer)
}
```

So the real system iterates over many stations, while the Lean proof zooms in on one currently relevant slot.  
This is a standard local-to-global relationship:

- if the slot-level read protocol is safe
- then repeatedly applying it across stations preserves that local safety discipline

### 11.5 Go executes the three-phase harvest protocol

The real `Harvest` path is exactly the Lean `GoPC` story:

1. `ScanSeq`
2. `ReadPayload`
3. `ValidateSeq`

Success corresponds to `go_validate_pass`.  
Failure corresponds to `go_validate_fail`.

### 11.6 A successful read becomes a JSONL line

The final output in `structure/jsonl.go` looks like:

```json
{"probe_id":...,"tid":...,"addr":"0x...","seq":...,"is_active":...,"ts":...}
```

Lean does not care about JSON syntax.  
It cares about the stronger semantic fact:

- is this committed record guaranteed to come from a stable slot version?

The two main theorems are exactly about that.

---

## 12. Which real-world assumptions the proof depends on

Formal proofs are powerful, but they are not magic.  
They rely on the implementation actually honoring the abstract protocol.

### 12.1 C++ must really follow "odd first, payload next, even last"

In the source this is expressed via release ordering:

- `SDK/c++/coroTracer.h:159`
- `SDK/c++/coroTracer.h:172`

The Lean proof does not simulate a full CPU memory model instruction by instruction.  
Instead, its `Step` relation assumes the implementation really follows this contract.

So the theorem is best read as:

- if the implementation obeys this write ordering discipline, then torn records are not committed

### 12.2 Go must read `seq` atomically

Relevant source lines:

- `structure/station.go:50`
- `structure/station.go:71`

```go
seq1 := atomic.LoadUint64(&slot.Seq)
seq2 := atomic.LoadUint64(&slot.Seq)
```

These atomic observations are the foundation of the reader-side reasoning.  
Lean does not explicitly write "Acquire" in the theorem statements, but the meaning of `go_scan` and `go_validate_*` depends on the fact that these are proper atomic observation points.

### 12.3 `lastSeen` must be tracked per slot

In the real code:

- `TracerEngine.lastSeen` has type `[][8]uint64`
- each station tracks eight slot-local `seq` values

Relevant locations:

- `engine/engine.go:33`
- `engine/engine.go:89`
- `structure/station.go:43`

Under the single-slot abstraction, this corresponds to Lean's:

```lean
go_last_seen : Nat -> Nat
```

### 12.4 Ring reuse must be paired with generation tracking

Because slots are reused in a ring, "I saw data in this slot" is not enough.  
The reader must know whether it is looking at the same generation or a newer one.  
That is exactly what `seq` is for.

In the implementation:

- the ring position comes from `event_count % 8`
- generation identity comes from the slot-local `seq`

In Lean, the same idea appears as:

- `cpp_idx := (cpp_idx + 1) % 8`
- a monotone `seq` discipline for the slot

---

## 13. What the proof does not cover

This section matters just as much as the positive results, because it defines the proof boundary.

### 13.1 It does not prove the cross-language ABI layout itself

The codebase clearly works hard to enforce layout:

- Go structs for `GlobalHeader`, `StationData`, and `Epoch`
- `alignas(1024)` and `alignas(64)` on the C++ side
- the documentation in `docs/cTP.md`

But the Lean file does not prove:

- that Go and C++ structure sizes always match perfectly
- that every compiler on every platform will honor the intended layout exactly the same way

That part is guaranteed by engineering discipline and careful source design, not by the current theorem statements.

### 13.2 It does not prove that mmap, socket setup, or tracer initialization always succeed

The proof does not model:

- `open`
- `mmap`
- `connect`
- `listen`
- `Accept`
- target-process launch

So it proves protocol correctness, not environment reliability.

### 13.3 It does not formalize `TracerSleeping` or UDS wakeup

The sleep/wakeup mechanism lives in:

- `engine/engine.go:126-162`
- `SDK/c++/coroTracer.h:176-180`

That mechanism is highly relevant in practice, but the current Lean proof does not cover it.  
So the current theorems do not by themselves imply:

- "the engine will always be woken immediately"
- "no wakeup can ever be missed at the transport level"

What they do imply is narrower and more fundamental:

- once Go actually enters the scan path and gets a stable observation window, committed data is safe; and if the window stays quiet long enough, one harvest attempt must succeed

### 13.4 It does not yet state the full global reachable-state theorem

Strictly speaking, `system_is_always_safe` is a one-step preservation theorem, not the final transitive closure over all executions.  
To package the whole story into the most complete form, one would usually also add:

1. a lemma showing the initial state satisfies `SystemInvariant`
2. an induction theorem over multi-step executions

The current file stops one layer below that, but it already contains the crucial inductive core.

---

## 14. Why this proof is still highly persuasive

Because it targets the hardest and most dangerous part of the system:

- when the writer and reader interleave on the same slot
- can Go ever commit a record that only looks valid but is actually stitched together from incompatible versions?

Without a formal argument, any statement like "this should probably be fine" remains fragile.  
`proof/proof.lean` turns that intuition into:

1. an explicit state machine
2. an explicit transition relation
3. an explicit invariant
4. an explicit case split over all legal steps

That means the central safety claim no longer rests on intuition alone.  
It rests on a machine-checkable proof structure.

---

## 15. A practical reading order

If you want to align the Lean proof with the implementation as efficiently as possible, the following order works well.

### Step one: read the real read/write protocol first

Start with:

- `SDK/c++/coroTracer.h:149-182`
- `structure/station.go:42-87`

The goal is to internalize the operational story:

- how the writer flips `seq` from even to odd and back
- how the reader observes `seq` twice

### Step two: read the Lean `Step` relation

Then read:

- `proof/proof.lean:38-97`

At that point you will see that the proof is almost a direct state-machine translation of the real protocol.

### Step three: read the safety theorem

Then read:

- `proof/proof.lean:106-191`

Pay special attention to:

- why only `go_validate_pass` can extend the log
- why the newly inserted log entry inherits the "even observed `seq`" fact

### Step four: read the liveness theorem

Finally read:

- `proof/proof.lean:197-250`

Pay special attention to:

- why three steps are enough
- why `go_validate_fail` becomes impossible under the quiet-window assumptions

---

## 16. Final summary

If we compress the whole proof into one sentence, it is this:

> coroTracer does not rely on "hopefully Go did not read in the middle of a write." Instead, it uses an explicit odd/even `seq` protocol plus a two-observation validation pattern on the Go side, and the Lean model proves that committed log records cannot come from half-written states; it also proves that if the writer briefly stops interfering, the reader must be able to complete one successful commit.

Expanded slightly, the project has three aligned layers:

1. **The C++ write protocol**  
   odd first, payload next, even last

2. **The Go read protocol**  
   check whether `seq` is new and stable, copy the payload, then validate that `seq` did not change

3. **The Lean proof**  
   the safety theorem guarantees "no torn committed data," and the liveness theorem guarantees "a quiet window is enough for one successful harvest"

Those three layers map directly to:

- `SDK/c++/coroTracer.h`
- `structure/station.go` and `structure/jsonl.go`
- `proof/proof.lean`

So this Lean proof is not a detached theoretical appendix.  
It is a formal explanation of the most important concurrency contract in the system.
