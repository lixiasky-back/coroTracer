import Init.Data.Nat.Basic

namespace CoroTracer

-- ==========================================
-- 1. 空间安全性 (Spatial Safety) 证明
-- ==========================================

theorem memory_bounds_safe (alloc max idx : Nat) (h_idx : idx < min alloc max) : idx < max := by
  have h_min_le_max : min alloc max ≤ max := Nat.min_le_right alloc max
  exact Nat.lt_of_lt_of_le h_idx h_min_le_max

-- ==========================================
-- 2. 内存序与脏读防护 (Weak Memory & No Dirty Reads)
-- ==========================================

inductive MemState where
  | Empty       : MemState
  | DataWritten : MemState
  | SeqReleased : MemState
  deriving Repr, DecidableEq

-- 修复：直接利用 DecidableEq 进行条件判断，抛弃不可计算的 Prop
def try_read_data (s : MemState) : Option String :=
  if s = MemState.SeqReleased then
    some "ValidData"
  else
    none

theorem no_dirty_reads (s : MemState) (read_result : Option String)
    (h_read : read_result = try_read_data s)
    (h_success : read_result = some "ValidData") :
    s = MemState.SeqReleased := by
  unfold try_read_data at h_read
  split at h_read
  · -- 分支 1：条件成立 (s = MemState.SeqReleased)
    rename_i h_eq
    exact h_eq
  · -- 分支 2：条件不成立 (s ≠ MemState.SeqReleased)
    -- 此时 read_result 必然是 none，将其与 h_success (some "ValidData") 结合产生数学矛盾
    rw [h_success] at h_read
    contradiction

-- ==========================================
-- 3. 活性与死锁防护 (Liveness & No Missed Wakeup)
-- ==========================================

structure SystemState where
  sleeping  : Bool
  hasData   : Bool
  udsSignal : Bool
  deriving Repr, DecidableEq

inductive Step : SystemState → SystemState → Prop where
  | TraceeWrite   (s) : Step s { s with hasData := true, udsSignal := s.udsSignal || s.sleeping }
  | TracerSleep   (s) : s.hasData = false → Step s { s with sleeping := true }
  | WakeByUDS     (s) : s.udsSignal = true → Step s { s with sleeping := false, udsSignal := false }
  | WakeByTimeout (s) : Step s { s with sleeping := false, udsSignal := false }
  | ConsumeData   (s) : s.hasData = true → s.sleeping = false → Step s { s with hasData := false }

inductive Reaches : SystemState → SystemState → Prop where
  | refl (s) : Reaches s s
  | step (s1 s2 s3) : Step s1 s2 → Reaches s2 s3 → Reaches s1 s3

theorem reaches_of_step {s1 s2 : SystemState} (h : Step s1 s2) : Reaches s1 s2 :=
  Reaches.step s1 s2 s2 h (Reaches.refl s2)

theorem eventual_progress (s : SystemState) (h_data : s.hasData = true) :
  ∃ s', Reaches s s' ∧ s'.hasData = false := by
  cases h_sleep : s.sleeping

  case false =>
    let s' := { s with hasData := false }
    have h_step : Step s s' := Step.ConsumeData s h_data h_sleep
    exact ⟨s', reaches_of_step h_step, rfl⟩

  case true =>
    let s_awake := { s with sleeping := false, udsSignal := false }
    have h_wake : Step s s_awake := Step.WakeByTimeout s

    let s_final := { s_awake with hasData := false }
    have h_data_awake : s_awake.hasData = true := by
      calc s_awake.hasData = s.hasData := rfl
      _ = true := h_data
    have h_sleep_awake : s_awake.sleeping = false := rfl
    have h_consume : Step s_awake s_final := Step.ConsumeData s_awake h_data_awake h_sleep_awake

    have h_reaches : Reaches s s_final :=
      Reaches.step s s_awake s_final h_wake (reaches_of_step h_consume)

    have h_done : s_final.hasData = false := rfl
    exact ⟨s_final, h_reaches, h_done⟩

end CoroTracer
