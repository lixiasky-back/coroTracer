import Init.Data.Nat.Basic

namespace CoroTracerDeepDive

-- ==========================================
-- 1. 核心状态空间
-- ==========================================
structure Slot where
  payload : Nat
  seq     : Nat
  deriving Repr, DecidableEq

inductive CppPC where
  | Idle
  | WritingPayload -- C++ 正在写入，此时共享内存的数据是不稳定的
  deriving DecidableEq, Repr

inductive GoPC where
  | ScanSeq      -- 第 1 步：读取并快照 Seq
  | ReadPayload  -- 第 2 步：读取 Payload 到本地
  | ValidateSeq  -- 第 3 步：回马枪！再次检查 Seq 防撕裂
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
  jsonl_log       : List (Nat × Nat) -- 记录最终被确认的安全数据 (Seq, Payload)

-- ==========================================
-- 2. 真实并发状态机 (允许套圈与撕裂发生)
-- ==========================================
inductive Step : SystemState → SystemState → Prop where

  -- 🔴 C++ 操作 1: 准备写入，Seq 递增变为奇数 (表示 Lock 状态)
  | cpp_start_write (s : SystemState) :
      s.cpp_pc = CppPC.Idle →
      Step s { s with
        cpp_pc := CppPC.WritingPayload,
        slots := fun i => if i = s.cpp_idx then { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 } else s.slots i
      }

  -- 🔴 C++ 操作 2: 极限并发写入 Payload（随时可能覆盖 Go 正在读的槽位）
  | cpp_write_data (s : SystemState) (new_data : Nat) :
      s.cpp_pc = CppPC.WritingPayload →
      Step s { s with
        slots := fun i => if i = s.cpp_idx then { payload := new_data, seq := (s.slots i).seq } else s.slots i
      }

  -- 🔴 C++ 操作 3: 写入完成，Seq 再次递增变为偶数 (表示 Unlock 状态)，并移动指针
  | cpp_end_write (s : SystemState) :
      s.cpp_pc = CppPC.WritingPayload →
      Step s { s with
        cpp_pc := CppPC.Idle,
        slots := fun i => if i = s.cpp_idx then { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 } else s.slots i,
        cpp_idx := (s.cpp_idx + 1) % 8
      }

  -- 🔵 Go 操作 1: 读取 Seq 快照。必须是偶数（非写入中）且大于 last_seen
  | go_scan (s : SystemState) :
      s.go_pc = GoPC.ScanSeq →
      Step s { s with
        go_pc := if (s.slots s.go_scan_idx).seq % 2 = 0 ∧ (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx
                 then GoPC.ReadPayload
                 else GoPC.ScanSeq,
        go_observed_seq := (s.slots s.go_scan_idx).seq
      }

  -- 🔵 Go 操作 2: 缓慢读取 Payload（注意：在此期间，C++ 的 step 随时可能触发，改写这个槽位）
  | go_read (s : SystemState) :
      s.go_pc = GoPC.ReadPayload →
      Step s { s with
        go_pc := GoPC.ValidateSeq,
        go_temp_payload := (s.slots s.go_scan_idx).payload
      }

  -- 🔵 Go 操作 3a: 读后校验成功！Seq 没有变，证明刚刚读 Payload 的时候 C++ 没有来捣乱，安全落盘。
  | go_validate_pass (s : SystemState) :
      s.go_pc = GoPC.ValidateSeq ∧ (s.slots s.go_scan_idx).seq = s.go_observed_seq →
      Step s { s with
        go_pc := GoPC.ScanSeq,
        go_last_seen := fun i => if i = s.go_scan_idx then s.go_observed_seq else s.go_last_seen i,
        jsonl_log := (s.go_observed_seq, s.go_temp_payload) :: s.jsonl_log
      }

  -- 🔵 Go 操作 3b: 读后校验失败！Seq 变了！说明发生了撕裂（Tearing）。直接丢弃临时数据，绝不更新 last_seen。
  | go_validate_fail (s : SystemState) :
      s.go_pc = GoPC.ValidateSeq ∧ (s.slots s.go_scan_idx).seq ≠ s.go_observed_seq →
      Step s { s with
        go_pc := GoPC.ScanSeq
        -- 核心防御机制：丢弃脏数据，重新进入 Scan 阶段
      }


-- ==========================================
-- 3. 核心安全性证明
-- ==========================================

-- 定理 1：系统永远不会把“撕裂”的、一半写一半没写的数据（奇数 Seq）记录到 JSONL 中
-- 1. 定义一个“健康系统”应该满足的复合不变量
def SystemInvariant (s : SystemState) : Prop :=
  -- 条件 A：日志里的数据都是偶数（无撕裂）
  (∀ seq pay, (seq, pay) ∈ s.jsonl_log → seq % 2 = 0) ∧
  -- 条件 B：只要 Go 开始读数据或校验数据，它攥在手里的快照必定是偶数
  (s.go_pc = GoPC.ReadPayload ∨ s.go_pc = GoPC.ValidateSeq → s.go_observed_seq % 2 = 0)

-- 2. 完美全覆盖证明
theorem system_is_always_safe (s1 s2 : SystemState) (h_step : Step s1 s2) :
    SystemInvariant s1 → SystemInvariant s2 := by
  intro h_inv
  rcases h_inv with ⟨h_log_even, h_obs_even⟩
  constructor

  -- =======================================
  -- 证明目标 A：所有写入 JSONL 的 Seq 都是偶数
  -- =======================================
  · intro seq pay h_in_s2
    cases h_step
    -- 对于没有修改 jsonl_log 的状态，直接继承前提 h_log_even
    case cpp_start_write => exact h_log_even seq pay h_in_s2
    case cpp_write_data => exact h_log_even seq pay h_in_s2
    case cpp_end_write => exact h_log_even seq pay h_in_s2
    case go_scan => exact h_log_even seq pay h_in_s2
    case go_read => exact h_log_even seq pay h_in_s2
    case go_validate_fail => exact h_log_even seq pay h_in_s2
    -- 只有 validate_pass 修改了 Log，需要特别处理
    case go_validate_pass h_cond =>
      -- 展开 ∈ 的定义，变成：(seq, pay) = 刚刚插入的元素 ∨ (seq, pay) 在之前的 log 中
      simp only [List.mem_cons] at h_in_s2
      cases h_in_s2 with
      | inl h_eq =>
        -- 情况 1：这是最新写进 Log 的那条数据
        -- 我们提取出目标 seq 等于 s1 阶段的 go_observed_seq
        injection h_eq with h_seq h_pay
        rw [h_seq]
        -- 利用条件 B：因为当时处于 ValidateSeq，所以拿着的 observed_seq 必为偶数
        apply h_obs_even
        right
        exact h_cond.left
      | inr h_mem =>
        -- 情况 2：这是以前的老数据，用前提秒杀
        exact h_log_even seq pay h_mem

  -- =======================================
  -- 证明目标 B：进入 Read/Validate 阶段时，攥在手里的 Seq 必为偶数
  -- =======================================
  · intro h_pc_s2
    cases h_step
    -- 以下操作不会改变 go_pc 也不改变 observed_seq，状态平移
    case cpp_start_write => apply h_obs_even; exact h_pc_s2
    case cpp_write_data => apply h_obs_even; exact h_pc_s2
    case cpp_end_write => apply h_obs_even; exact h_pc_s2

    -- go_scan 阶段最核心的 if 分支推导
    case go_scan h_pc =>
      dsimp at h_pc_s2 ⊢ -- 展开状态更新
      split at h_pc_s2
      · rename_i h_if
        -- If 判定为 True，说明读到了新的数据。
        -- 此时目标变成了证明 (s1.slots s1.go_scan_idx).seq 是偶数，这刚好在 h_if.left 里！
        exact h_if.left
      · rename_i h_if
        -- If 判定为 False，系统停留在 ScanSeq 状态。
        -- h_pc_s2 说它是 ReadPayload 或 ValidateSeq，产生矛盾！拆解后击杀。
        cases h_pc_s2 with
        | inl h1 => contradiction
        | inr h2 => contradiction

    -- 从 Read 进入 Validate 阶段
    case go_read h_pc =>
      apply h_obs_even
      left
      exact h_pc

    -- 以下两个状态会重置回 ScanSeq，显然矛盾
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

end CoroTracerDeepDive
