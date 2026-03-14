import Init.Data.Nat.Basic

namespace CoroTracerUnified

structure Slot where
  payload : Nat
  seq     : Nat
  deriving Repr, DecidableEq

inductive CppPC where | Idle | PublishSeq | Fence deriving DecidableEq, Repr
inductive GoPC where | Scan | ReadPayload | SetSleep | DoubleCheck | Sleeping deriving DecidableEq, Repr

structure SystemState where
  slots          : Nat → Slot
  shm_sleeping   : Nat
  shm_is_dead    : Bool
  cpp_pc         : CppPC
  cpp_local_seq  : Nat
  cpp_payload_in : Nat
  go_pc          : GoPC
  go_scan_idx    : Nat
  go_last_seen   : Nat → Nat
  go_observed_seq: Nat
  jsonl_log      : List Nat

inductive Step : SystemState → SystemState → Prop where
  | cpp_write_payload (s : SystemState) :
      s.cpp_pc = CppPC.Idle →
      Step s { s with
        cpp_pc := CppPC.PublishSeq,
        slots := fun i => if i = (s.cpp_local_seq + 1) % 8 then { payload := s.cpp_payload_in, seq := (s.slots i).seq } else s.slots i
      }
  | cpp_publish_seq (s : SystemState) :
      s.cpp_pc = CppPC.PublishSeq →
      Step s { s with
        cpp_pc := CppPC.Fence,
        cpp_local_seq := s.cpp_local_seq + 1,
        slots := fun i => if i = (s.cpp_local_seq + 1) % 8 then { payload := (s.slots i).payload, seq := s.cpp_local_seq + 1 } else s.slots i
      }
  | cpp_fence (s : SystemState) :
      s.cpp_pc = CppPC.Fence →
      Step s { s with cpp_pc := CppPC.Idle, shm_sleeping := if s.shm_sleeping = 1 then 0 else s.shm_sleeping }
  | go_scan_slot (s : SystemState) :
      s.go_pc = GoPC.Scan →
      Step s { s with
        go_pc := if (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx then GoPC.ReadPayload else GoPC.SetSleep,
        go_observed_seq := (s.slots s.go_scan_idx).seq
      }
  | go_read_payload (s : SystemState) :
      s.go_pc = GoPC.ReadPayload →
      Step s { s with
        go_pc := GoPC.Scan,
        go_last_seen := fun i => if i = s.go_scan_idx then max s.go_observed_seq (s.go_last_seen i) else s.go_last_seen i,
        jsonl_log := (s.slots s.go_scan_idx).payload :: s.jsonl_log
      }
  | go_set_sleep (s : SystemState) :
      s.go_pc = GoPC.SetSleep →
      Step s { s with go_pc := GoPC.DoubleCheck, shm_sleeping := 1 }
  | go_double_check (s : SystemState) :
      s.go_pc = GoPC.DoubleCheck →
      Step s { s with
        go_pc := if (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx then GoPC.Scan else GoPC.Sleeping,
        shm_sleeping := if (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx then 0 else s.shm_sleeping
      }


theorem deep_ring_buffer_safe (s : SystemState) : s.cpp_local_seq % 8 < 8 := by
  apply Nat.mod_lt; decide

-- 🔴 核心变化：强制使用 7 个点（·）对应 7 种状态变化，杜绝目标丢失
theorem deep_monotonicity (s1 s2 : SystemState) (h_step : Step s1 s2) (i : Nat) :
    s1.go_last_seen i ≤ s2.go_last_seen i := by
  cases h_step
  · simp_all
  · simp_all
  · simp_all
  · simp_all
  · by_cases h : i = s1.go_scan_idx
    · simp_all; exact Nat.le_max_right _ _
    · simp_all
  · simp_all
  · simp_all

-- 以下可以直接用 simp_all 秒杀的，就不展开写点了
theorem deep_wakeup_resolution (s1 s2 : SystemState) (h_step : Step s1 s2)
    (h_cpp_was_fence : s1.cpp_pc = CppPC.Fence)
    (h_cpp_now_idle : s2.cpp_pc = CppPC.Idle)
    (h_go_sleep : s1.shm_sleeping = 1) :
    s2.shm_sleeping = 0 := by
  cases h_step <;> simp_all

theorem deep_shm_safe (s1 s2 : SystemState) (h_step : Step s1 s2) (h_dead : s1.shm_is_dead = true) :
    s2.shm_is_dead = true := by
  cases h_step <;> simp_all

theorem deep_data_loss_boundary (s : SystemState) (i : Nat) (h_fast : s.cpp_local_seq > s.go_last_seen i + 8) :
    s.cpp_local_seq - s.go_last_seen i > 8 := by
  omega

theorem deep_wait_free (s1 s2 : SystemState) (h_step : Step s1 s2)
    (h_cpp_was_pub : s1.cpp_pc = CppPC.PublishSeq)
    (h_pc_changed : s1.cpp_pc ≠ s2.cpp_pc) :
    s2.cpp_pc = CppPC.Fence := by
  cases h_step <;> simp_all

theorem deep_jsonl_consistency (s1 s2 : SystemState) (h_step : Step s1 s2)
    (h_log_changed : s1.jsonl_log ≠ s2.jsonl_log) :
    s2.jsonl_log = (s1.slots s1.go_scan_idx).payload :: s1.jsonl_log := by
  cases h_step <;> simp_all

-- 🔴 精确制导：拆分 7 个分支，只在必要的两个 C++ 操作里切分 if 逻辑
theorem deep_no_dirty_read_release (s1 s2 : SystemState) (h_step : Step s1 s2)
    (idx : Nat)
    (h_payload_changed : (s1.slots idx).payload ≠ (s2.slots idx).payload) :
    (s2.slots idx).seq = (s1.slots idx).seq := by
  cases h_step
  · by_cases h : idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · by_cases h : idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · simp_all
  · simp_all
  · simp_all
  · simp_all
  · simp_all

theorem deep_snapshot_consistency (s1 s2 : SystemState) (h_step : Step s1 s2)
    (h_go_entering_read : s1.go_pc = GoPC.Scan ∧ s2.go_pc = GoPC.ReadPayload) :
    s2.go_observed_seq > s1.go_last_seen s1.go_scan_idx := by
  rcases h_go_entering_read with ⟨h1, h2⟩
  cases h_step
  · simp_all
  · simp_all
  · simp_all
  · by_cases h : (s1.slots s1.go_scan_idx).seq > s1.go_last_seen s1.go_scan_idx <;> simp_all
  · simp_all
  · simp_all
  · simp_all

theorem deep_slot_isolation (s1 s2 : SystemState) (h_step : Step s1 s2)
    (target_idx : Nat)
    (h_diff_slot : target_idx ≠ (s1.cpp_local_seq + 1) % 8) :
    (s2.slots target_idx).payload = (s1.slots target_idx).payload := by
  cases h_step
  · by_cases h : target_idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · by_cases h : target_idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · simp_all
  · simp_all
  · simp_all
  · simp_all
  · simp_all

theorem deep_end_to_end_tearing_freedom (s1 s2 : SystemState) (h_step : Step s1 s2)
    (h_go_reading : s1.go_pc = GoPC.ReadPayload)
    -- 物理大前提：环形队列的安全距离没有被打破（即 C++ 即将写入的下一个槽位，不是 Go 正在读的槽位）
    (h_safe_distance : (s1.cpp_local_seq + 1) % 8 ≠ s1.go_scan_idx) :
    -- 结论：无论发生什么状态转换，Go 正在扫描的槽位 Payload 绝对不变
    (s2.slots s1.go_scan_idx).payload = (s1.slots s1.go_scan_idx).payload := by
  cases h_step
  · -- cpp_write_payload 分支
    by_cases h : s1.go_scan_idx = (s1.cpp_local_seq + 1) % 8
    · -- 如果 C++ 刚好写到了 Go 在读的槽位，这与我们的安全距离大前提矛盾
      simp_all
    · -- 正常情况：C++ 写的是其他槽位，Go 读的槽位安然无恙
      simp_all
  · -- cpp_publish_seq 分支
    by_cases h : s1.go_scan_idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · -- cpp_fence 分支
    simp_all
  · -- go_scan_slot 分支 (状态互斥，直接消灭)
    simp_all
  · -- go_read_payload 分支
    simp_all
  · -- go_set_sleep 分支
    simp_all
  · -- go_double_check 分支
    simp_all

-- ==========================================
-- 🛡️ 附属保障定理：Seq 与 Payload 强绑定 (Data/Version Consistency)
-- 证明：如果 Go 决定进入 ReadPayload，说明它看到的 Seq 是新的。
-- 在读取期间，这个 Seq 绝对不可能被其他并发操作悄悄回退或篡改（除非发生追尾）。
-- ==========================================
theorem deep_read_acquire_security (s1 s2 : SystemState) (h_step : Step s1 s2)
    (h_go_reading : s1.go_pc = GoPC.ReadPayload)
    (h_safe_distance : (s1.cpp_local_seq + 1) % 8 ≠ s1.go_scan_idx) :
    (s2.slots s1.go_scan_idx).seq = (s1.slots s1.go_scan_idx).seq := by
  cases h_step
  · by_cases h : s1.go_scan_idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · by_cases h : s1.go_scan_idx = (s1.cpp_local_seq + 1) % 8 <;> simp_all
  · simp_all
  · simp_all
  · simp_all
  · simp_all
  · simp_all

end CoroTracerUnified
