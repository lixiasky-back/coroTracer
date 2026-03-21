import Init.Data.Nat.Basic

namespace CoroTracerDeepDive

-- ==========================================
-- 1. Core state space
-- ==========================================
structure Slot where
  payload : Nat
  seq     : Nat
  deriving Repr, DecidableEq

inductive CppPC where
  | Idle
  | WritingPayload -- C++ is writing, shared memory data is unstable
  deriving DecidableEq, Repr

inductive GoPC where
  | ScanSeq      -- Step 1: Read and snapshot Seq
  | ReadPayload  -- Step 2: Read Payload locally
  | ValidateSeq  -- Step 3: Check Seq again to prevent tearing
  deriving DecidableEq, Repr

structure SystemState where
  slots           : Nat → Slot
  cpp_pc          : CppPC
  cpp_idx         : Nat
  go_pc           : GoPC
  go_scan_idx     : Nat
  go_last_seen    : Nat → Nat
  go_observed_seq : Nat
  go_temp_payload : Nat
  jsonl_log       : List (Nat × Nat) -- Record the final confirmed safe data (Seq, Payload)

-- ==========================================
-- 2. Real Concurrent State Machine (Allows wrap-around and tearing)
-- ==========================================
inductive Step : SystemState → SystemState → Prop where

  -- 🔴 C++ Operation 1: Prepare to write, increment Seq to odd (Lock state)
  | cpp_start_write (s : SystemState) :
      s.cpp_pc = CppPC.Idle →
      Step s { s with
        cpp_pc := CppPC.WritingPayload,
        slots := fun i => if i = s.cpp_idx then { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 } else s.slots i
      }

  -- 🔴 C++ Operation 2: High-concurrency Payload write (may overwrite the slot being read by Go at any time)
  | cpp_write_data (s : SystemState) (new_data : Nat) :
      s.cpp_pc = CppPC.WritingPayload →
      Step s { s with
        slots := fun i => if i = s.cpp_idx then { payload := new_data, seq := (s.slots i).seq } else s.slots i
      }

  -- 🔴 C++ Operation 3: Write completed, increment Seq to even (Unlock state) and move the pointer
  | cpp_end_write (s : SystemState) :
      s.cpp_pc = CppPC.WritingPayload →
      Step s { s with
        cpp_pc := CppPC.Idle,
        slots := fun i => if i = s.cpp_idx then { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 } else s.slots i,
        cpp_idx := (s.cpp_idx + 1) % 8
      }

  -- 🔵 Go Operation 1: Read Seq snapshot. Must be even (not writing) and greater than last_seen
  | go_scan (s : SystemState) :
      s.go_pc = GoPC.ScanSeq →
      Step s { s with
        go_pc := if (s.slots s.go_scan_idx).seq % 2 = 0 ∧ (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx
                 then GoPC.ReadPayload
                 else GoPC.ScanSeq,
        go_observed_seq := (s.slots s.go_scan_idx).seq
      }

  -- 🔵 Go Operation 2: Read Payload slowly (Note: C++ steps may trigger at any time and overwrite this slot during this period)
  | go_read (s : SystemState) :
      s.go_pc = GoPC.ReadPayload →
      Step s { s with
        go_pc := GoPC.ValidateSeq,
        go_temp_payload := (s.slots s.go_scan_idx).payload
      }

  -- 🔵 Go Operation 3a: Post-read validation passed! Seq unchanged, confirming C++ did not interfere during the Payload read. Safe to commit.
  | go_validate_pass (s : SystemState) :
      s.go_pc = GoPC.ValidateSeq ∧ (s.slots s.go_scan_idx).seq = s.go_observed_seq →
      Step s { s with
        go_pc := GoPC.ScanSeq,
        go_last_seen := fun i => if i = s.go_scan_idx then s.go_observed_seq else s.go_last_seen i,
        jsonl_log := (s.go_observed_seq, s.go_temp_payload) :: s.jsonl_log
      }

 -- 🔵 Go Operation 3b: Post-read validation failed! Seq changed! Tearing occurred. Discard temporary data directly and **never update last_seen.
  | go_validate_fail (s : SystemState) :
      s.go_pc = GoPC.ValidateSeq ∧ (s.slots s.go_scan_idx).seq ≠ s.go_observed_seq →
      Step s { s with
        go_pc := GoPC.ScanSeq
        -- Core Defense Mechanism: Discard dirty data and re-enter the scan phase
      }


-- ==========================================
-- 3. Core Security Proof
-- ==========================================

-- Theorem 1: The system will never record "torn", half-written data (odd Seq) into JSONL
-- 1. Define the composite invariant that a "healthy system" must satisfy
def SystemInvariant (s : SystemState) : Prop :=
  -- Condition A: All data in the log are even-numbered (no tearing)
  (∀ seq pay, (seq, pay) ∈ s.jsonl_log → seq % 2 = 0) ∧
  -- Condition B: Whenever Go starts reading or validating data, the snapshot it holds must be even
  (s.go_pc = GoPC.ReadPayload ∨ s.go_pc = GoPC.ValidateSeq → s.go_observed_seq % 2 = 0)

-- 2. Comprehensive Proof
theorem system_is_always_safe (s1 s2 : SystemState) (h_step : Step s1 s2) :
    SystemInvariant s1 → SystemInvariant s2 := by
  intro h_inv
  rcases h_inv with ⟨h_log_even, h_obs_even⟩
  constructor

  -- =======================================
  -- Proof Goal A: All Seq values written to JSONL are even
  -- =======================================
  · intro seq pay h_in_s2
    cases h_step
    -- For states where jsonl_log is unmodified, directly inherit the premise h_log_even
    case cpp_start_write => exact h_log_even seq pay h_in_s2
    case cpp_write_data => exact h_log_even seq pay h_in_s2
    case cpp_end_write => exact h_log_even seq pay h_in_s2
    case go_scan => exact h_log_even seq pay h_in_s2
    case go_read => exact h_log_even seq pay h_in_s2
    case go_validate_fail => exact h_log_even seq pay h_in_s2
    -- Only validate_pass modifies the log, requiring special handling
    case go_validate_pass h_cond =>
      -- Expand the definition of ∈ to: (seq, pay) = the newly inserted element ∨ (seq, pay) is in the previous log
      simp only [List.mem_cons] at h_in_s2
      cases h_in_s2 with
      | inl h_eq =>
        -- Case 1: This is the latest data written to the Log
        -- We derive that the target seq is equal to go_observed_seq in stage s1
        injection h_eq with h_seq h_pay
        rw [h_seq]
        -- Apply Condition B: Since it is in the ValidateSeq phase, the held observed_seq must be even
        apply h_obs_even
        right
        exact h_cond.left
      | inr h_mem =>
        -- Case 2: This is old existing data, resolved directly by the premise
        exact h_log_even seq pay h_mem

  -- =======================================
  -- Proof Goal B: When entering the Read/Validate phase, the Seq held must be even
  -- =======================================
  · intro h_pc_s2
    cases h_step
    -- The following operations do not change go_pc or observed_seq, state transition without modification
    case cpp_start_write => apply h_obs_even; exact h_pc_s2
    case cpp_write_data => apply h_obs_even; exact h_pc_s2
    case cpp_end_write => apply h_obs_even; exact h_pc_s2

    -- Core if-branch derivation in the go_scan phase
    case go_scan h_pc =>
      dsimp at h_pc_s2 ⊢ -- Expand state updates
      split at h_pc_s2
      · rename_i h_if
        -- If condition is True, new data has been read.
        -- The goal now becomes proving (s1.slots s1.go_scan_idx).seq is even, which is exactly in h_if.left!
        exact h_if.left
      · rename_i h_if
        -- If condition is False, the system remains in ScanSeq state.
        -- h_pc_s2 states it is ReadPayload or ValidateSeq, resulting in a contradiction! Resolve by decomposition.
        cases h_pc_s2 with
        | inl h1 => contradiction
        | inr h2 => contradiction

    -- Transition from Read phase to Validate phase
    case go_read h_pc =>
      apply h_obs_even
      left
      exact h_pc

    -- The following two states reset back to ScanSeq, which is clearly a contradiction
    case go_validate_pass h_cond =>
      dsimp at h_pc_s2
      cases h_pc_s2 with
      | inl h1 => contradiction
      | inr h2 => contradiction

    case go_validate_fail h_cond =>
      dsimp at h_pc_s2
      cases h_pc_s2 with
      | inl h1 => contradiction
      | inr h2 => contradiction

-- ==========================================
-- 4. Accessibility Validity Certificate
-- ==========================================

theorem go_obstruction_free_liveness
    (s0 s1 s2 s3 : SystemState)
    (step1 : Step s0 s1)
    (step2 : Step s1 s2)
    (step3 : Step s2 s3)
    (h_go_pc : s0.go_pc = GoPC.ScanSeq)
    (h_new_data : (s0.slots s0.go_scan_idx).seq > s0.go_last_seen s0.go_scan_idx)
    (h_even : (s0.slots s0.go_scan_idx).seq % 2 = 0)
    -- Core Fix: Must provide silent guarantee throughout the entire 3-step cycle!
    (h_quiet0 : s0.cpp_pc = CppPC.Idle)
    (h_quiet1 : s1.cpp_pc = CppPC.Idle)
    (h_quiet2 : s2.cpp_pc = CppPC.Idle)
    (h_quiet3 : s3.cpp_pc = CppPC.Idle) :
    s3.go_pc = GoPC.ScanSeq ∧
    (s3.go_observed_seq, s3.go_temp_payload) ∈ s3.jsonl_log := by

  -- [Step 1: Deduction from s0 to s1]
  cases step1
  case cpp_start_write => contradiction
  case cpp_write_data new_data h => rw [h_quiet0] at h; contradiction
  case cpp_end_write h => rw [h_quiet0] at h; contradiction
  case go_read h => rw [h_go_pc] at h; contradiction
  case go_validate_pass h => have hp := h.left; rw [h_go_pc] at hp; contradiction
  case go_validate_fail h => have hp := h.left; rw [h_go_pc] at hp; contradiction
  case go_scan h_pc =>
    have h_if : (s0.slots s0.go_scan_idx).seq % 2 = 0 ∧ (s0.slots s0.go_scan_idx).seq > s0.go_last_seen s0.go_scan_idx := ⟨h_even, h_new_data⟩
    have h_s1_pc_is_read : (if (s0.slots s0.go_scan_idx).seq % 2 = 0 ∧ (s0.slots s0.go_scan_idx).seq > s0.go_last_seen s0.go_scan_idx then GoPC.ReadPayload else GoPC.ScanSeq) = GoPC.ReadPayload := by simp [h_if]

    -- [Step 2: Deduction from s1 to s2]
    cases step2
    case cpp_start_write => contradiction
    -- Adopt the silence assumption at moment 1
    case cpp_write_data new_data h => rw [h_quiet1] at h; contradiction
    case cpp_end_write h => rw [h_quiet1] at h; contradiction
    case go_scan h => rw [h_s1_pc_is_read] at h; contradiction
    case go_validate_pass h => have hp := h.left; rw [h_s1_pc_is_read] at hp; contradiction
    case go_validate_fail h => have hp := h.left; rw [h_s1_pc_is_read] at hp; contradiction
    case go_read h2 =>

      -- [Step 3: Deduction from s2 to s3]
      cases step3
      case cpp_start_write => contradiction
      -- Adopt the silence assumption at moment 2
      case cpp_write_data new_data h => rw [h_quiet2] at h; contradiction
      case cpp_end_write h => rw [h_quiet2] at h; contradiction
      case go_scan h => contradiction
      case go_read h => contradiction
      case go_validate_fail h =>
        have hp := h.right
        exact False.elim (hp rfl)
      case go_validate_pass _ =>
        constructor
        · rfl
        · simp [List.mem_cons]

end CoroTracerDeepDive
