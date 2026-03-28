# coroTracer 的 Lean 4 证明详解

本文档详细解释 [`proof/proof.lean`](proof/proof.lean) 在证明什么、为什么这样建模，以及它和 Go/C++ 源码之间的逐段对应关系。

如果你第一次看这个仓库，建议先打开下面几个文件对照阅读：

- `proof/proof.lean`：形式化证明本体
- `SDK/c++/coroTracer.h`：C++ 写侧，负责把协程状态写入共享内存
- `structure/station.go`：Go 读侧，负责无锁扫描共享内存
- `structure/jsonl.go`：Go 侧把确认安全的数据落到 JSONL
- `engine/engine.go`：Go 引擎主循环、休眠和 UDS 唤醒
- `docs/cTP.md`：协议层面的文字说明

---

## 1. 先说结论：Lean 到底证明了什么

这份 Lean 证明主要覆盖两件事。

### 1.1 安全性：Go 不会把“半写入”的脏数据写进日志

更准确地说，它证明了：

1. Go 侧只有在 `seq` 为偶数且前后两次读取相等时，才会提交本次读取结果。
2. 因此，被提交到日志里的记录不可能来自“C++ 正在写一半”的中间状态。
3. 也就是说，日志里不会出现“撕裂读（torn read）”。

这对应源码里的核心逻辑是：

- C++ 写侧先把 `seq` 改成奇数，表示“正在写”
- C++ 写完 payload 后再把 `seq` 改成偶数，表示“写完了”
- Go 读侧先读一次 `seq`
- 如果发现是奇数，直接跳过
- 如果是偶数，就把 payload 拷到本地
- 然后再读一次 `seq`
- 如果两次 `seq` 不一样，说明中间被并发写打断，丢弃这次读取

### 1.2 活性：只要 C++ 暂时不干扰，Go 最终能成功收割一条记录

更准确地说，它证明了一个“无阻塞窗口”下的活性结论：

1. 假设某个 slot 里已经有一条新的、稳定的、偶数 `seq` 数据。
2. 假设 Go 当前正处于扫描开始阶段。
3. 假设在 Go 的“扫描 -> 读取 payload -> 验证”这三个步骤期间，C++ 没有再来覆盖这个 slot。
4. 那么 Go 最终一定会把这条记录提交到日志里。

注意，这里证明的是一种 **obstruction-free liveness**，不是“无论任何并发干扰都必然一步成功”。  
它的意思是：只要写侧给读侧留出一个足够短的安静窗口，读侧就能完成一次成功提交。

---

## 2. 为什么这个证明重要

这个项目的核心不是“能不能读到数据”，而是“读到的数据到底是不是一个完整、真实、未撕裂的状态切片”。

因为真实系统里存在下面这个竞争：

1. C++ 正在往共享内存里的某个 slot 写入新状态。
2. Go 同时也在读这个 slot。
3. 如果 Go 在 C++ 写了一半时恰好把数据读走，就可能得到一条拼接出来的假记录：
   - `tid` 来自旧版本
   - `addr` 来自新版本
   - `timestamp` 又来自另一轮
4. 这种记录不会触发内存安全错误，但会把排障结论带偏。

这类错误非常隐蔽，因为：

- 地址没有越界
- 没有 use-after-free
- 也不一定会触发 data race 检测工具直接报错
- 但日志语义已经坏掉了

`proof/proof.lean` 的价值就在这里：它把“日志里不会出现这种半新半旧的伪记录”这件事形式化了。

---

## 3. 真实系统里的协议长什么样

在看 Lean 之前，先把真实代码里的协议用最短路径看一遍。

### 3.1 C++ 写侧：先奇数，再写 payload，最后偶数

关键代码在 `SDK/c++/coroTracer.h:149-182`，函数是 `PromiseMixin::write_trace`。

逻辑可以概括成下面的伪代码：

```cpp
slot = my_station->slots[event_count % 8]
old_seq = slot.seq.load(relaxed)

slot.seq.store(old_seq + 1, release)   // 奇数：正在写

slot.addr = ...
slot.tid = ...
slot.timestamp = ...
slot.is_active = ...

slot.seq.store(old_seq + 2, release)   // 偶数：写完了
event_count++
```

其中有几个要点：

1. `event_count % 8` 说明每个协程 station 内部是一个 8 槽位的 ring。
2. `old_seq + 1` 把 `seq` 变成奇数，用来告诉 Go：“现在不要读我。”
3. 真正的 payload 写入发生在两次 `seq` store 之间。
4. `old_seq + 2` 把 `seq` 变回偶数，表示一整个写事务已经结束。
5. 最后 `event_count++`，下一次会去下一个 ring slot。

### 3.2 Go 读侧：先读 seq，再读 payload，再复核 seq

关键代码在 `structure/station.go:42-87`，函数是 `(*StationData).Harvest`。

核心逻辑可以概括成：

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

这里的三个阶段非常关键：

1. `ScanSeq`：先看有没有新数据，以及当前是不是稳定态。
2. `ReadPayload`：把共享内存字段拷到本地变量。
3. `ValidateSeq`：再读一次 `seq`，确认这次读取没有被打断。

Lean 里的 `GoPC` 三态，正是对这三步的直接抽象。

### 3.3 日志提交点：只有验证通过才落盘

关键代码在 `structure/jsonl.go:73-76`：

```go
sw.line = s.marshalSafeSlotJSONL(...)
_, err := sw.writer.Write(sw.line)
```

这一步只会发生在 `Harvest` 中 `seq1 == seq2` 的分支里。  
所以，只要我们证明“能进到这里的 `seq` 一定不是脏的”，日志就是可信的。

### 3.4 Go 引擎主循环：负责不断调用 Harvest

`engine/engine.go` 里的职责更偏工程层：

- `NewTracerEngine` 建 SHM 和 UDS
- `doScan` 遍历已分配的 station 并调用 `Harvest`
- `hotHarvestLoop` 在“忙扫描”和“休眠等唤醒”之间切换

注意：Lean 证明并没有直接形式化 UDS、socket、mmap、子进程拉起等工程问题。  
它聚焦的是更窄、也更核心的一层：**一个 slot 的并发读写协议本身是否正确**。

---

## 4. Lean 模型如何抽象真实系统

下面是理解整份证明最重要的一张对照表。

| Lean 中的对象 | 含义 | 源码对应 | 为什么这样抽象 |
| --- | --- | --- | --- |
| `Slot.payload` | 某个 slot 的 payload | Go 里是 `tid/addr/is_active/timestamp`；C++ 里也是这些字段 | Lean 把多字段打包成一个抽象 payload，专注证明“是否读到完整版本” |
| `Slot.seq` | slot 的序列号 | `Epoch.Seq` / `Epoch::seq` | 这是并发协议真正的判定核心 |
| `CppPC.Idle` | C++ 当前没有在写这个 slot | `write_trace` 还未进入写事务或已经写完 | 用程序计数器描述写侧阶段 |
| `CppPC.WritingPayload` | C++ 已把 `seq` 设为奇数，正在写 payload | `write_trace` 两次 `slot.seq.store(...)` 之间 | 这是 Go 必须跳过的危险窗口 |
| `GoPC.ScanSeq` | Go 正在做第一次 `seq` 观察 | `seq1 := atomic.LoadUint64(...)` 与后续判断 | 读侧第 1 步 |
| `GoPC.ReadPayload` | Go 认定这个 slot 值得尝试读取 | 拷贝 `TID/Addr/IsActive/Timestamp` | 读侧第 2 步 |
| `GoPC.ValidateSeq` | Go 已经把 payload 拷到局部变量，准备复核 | `seq2 := atomic.LoadUint64(...)` | 读侧第 3 步 |
| `cpp_idx` | 当前写侧要写哪个 ring slot | `event_count % 8` | Lean 用显式索引来表达 ring 前进 |
| `go_last_seen` | Go 记住每个 slot 上次已成功提交的 `seq` | `TracerEngine.lastSeen [][8]uint64` | 用于过滤旧数据和重复数据 |
| `jsonl_log` | 已提交的安全记录 | `WriteSafeSlot` 写出的 JSONL 行 | Lean 不关心具体文件，只关心“提交集合” |

### 4.1 Lean 为什么把真实 payload 抽象成一个 `Nat`

在真实实现里，一个记录包含至少这几个字段：

- `tid`
- `addr`
- `timestamp`
- `is_active`

Lean 并没有逐字段建模，而是把它们合成了一个 `payload : Nat`。这不是偷懒，而是一种标准抽象：

1. 证明关心的不是“字段值具体是什么”。
2. 证明关心的是“Go 最终提交的 payload 是否来自某个完整写入版本”。
3. 只要这个结论对单个抽象 payload 成立，就能迁移到“多字段整体快照”的现实实现。

换句话说，Lean 证明的是：

- 不是 `tid` 单独安全
- 也不是 `addr` 单独安全
- 而是“这次被提交的整份 payload 快照一定是某个稳定版本”

这恰好就是工程上真正需要的性质。

### 4.2 Lean 为什么没有直接建模 station、header、socket

因为这份证明的焦点是 **slot 级别的并发协议**，不是整个系统所有工程细节。

它没有形式化下面这些内容：

- `GlobalHeader` 的跨语言布局
- `AllocatedCount` 的分配逻辑
- `TracerSleeping` 与 UDS 唤醒
- `InitTracer()` 的 mmap/UDS 连接过程
- `main.go` 里拉起子进程和注入环境变量

这些都很重要，但它们不是“撕裂读是否会发生”的核心。  
Lean 证明抓的是最底层的正确性内核：**C++ 按这个 seq 协议写，Go 按这个 seq 协议读，能不能保证日志没有半写入数据。**

---

## 5. 先把 Lean 文件按结构拆开

`proof/proof.lean` 可以分成四段。

### 5.1 第一段：定义状态空间

位置：`proof/proof.lean:8-33`

这里定义了：

- `Slot`
- `CppPC`
- `GoPC`
- `SystemState`

这是“整个证明世界里的宇宙”。

### 5.2 第二段：定义状态转移规则

位置：`proof/proof.lean:38-97`

这里定义了 `inductive Step : SystemState → SystemState → Prop`，也就是：

- 从一个系统状态到另一个系统状态，允许发生哪些原子步骤
- 每个步骤会如何修改系统

这是证明的核心。  
后面的定理都是围绕“任意一步 Step 会不会破坏我们想要的性质”来展开。

### 5.3 第三段：安全性证明

位置：`proof/proof.lean:104-191`

这里先定义了不变量 `SystemInvariant`，再证明：

- 如果某个状态满足这个不变量
- 那么经过任意一步 `Step`
- 新状态仍然满足这个不变量

### 5.4 第四段：活性证明

位置：`proof/proof.lean:197-250`

这里证明的是：

- 如果一开始就有一条新的稳定数据
- 且 Go 连续完成“扫描 -> 读取 -> 验证”
- 且这期间 C++ 没有插手
- 那么这条数据一定会进日志

---

## 6. 逐条解释 `Step`

这一节最关键。只要把 `Step` 每个构造子看懂，后面的定理就自然了。

### 6.1 `cpp_start_write`

位置：`proof/proof.lean:41-46`

Lean 定义：

```lean
| cpp_start_write (s : SystemState) :
    s.cpp_pc = CppPC.Idle →
    Step s { s with
      cpp_pc := CppPC.WritingPayload,
      slots := fun i =>
        if i = s.cpp_idx then
          { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 }
        else s.slots i
    }
```

含义非常直接：

1. 只有当 C++ 当前处于 `Idle` 时，才能开始写。
2. 一旦开始写，就把程序计数器切到 `WritingPayload`。
3. 同时把当前 slot 的 `seq` 加 1。

由于旧的稳定 `seq` 应该是偶数，所以这一步会把它改成奇数。  
奇数代表：**这个 slot 正在被写，读侧不要碰。**

源码对应：

- `SDK/c++/coroTracer.h:157-159`

```cpp
uint64_t old_seq = slot.seq.load(std::memory_order_relaxed);
slot.seq.store(old_seq + 1, std::memory_order_release);
```

为什么这是正确抽象：

- Lean 不关心 `old_seq` 是怎么来的，只关心“进入写阶段时，seq 会先变奇数”
- 这正是 Go 侧跳过不稳定数据的判据

### 6.2 `cpp_write_data`

位置：`proof/proof.lean:49-53`

Lean 定义：

```lean
| cpp_write_data (s : SystemState) (new_data : Nat) :
    s.cpp_pc = CppPC.WritingPayload →
    Step s { s with
      slots := fun i =>
        if i = s.cpp_idx then
          { payload := new_data, seq := (s.slots i).seq }
        else s.slots i
    }
```

含义：

1. 只有在 `WritingPayload` 状态下才允许写 payload。
2. 这一步只更新 payload，不更新 `seq`。
3. 所以整个写入中间窗口里，`seq` 一直保持奇数。

源码对应：

- `SDK/c++/coroTracer.h:164-167`

```cpp
slot.addr = addr;
slot.tid = get_tid();
slot.timestamp = get_ns();
slot.is_active = is_active;
```

注意这里 Lean 做了一个重要抽象：  
真实代码有多个字段，但 Lean 把它们视为一次 payload 更新。

这并不影响证明目标，因为我们真正关心的是：

- Go 是否会在这个“中间写阶段”错误提交数据

而不是字段拆开后每个字段的业务意义。

### 6.3 `cpp_end_write`

位置：`proof/proof.lean:56-62`

Lean 定义：

```lean
| cpp_end_write (s : SystemState) :
    s.cpp_pc = CppPC.WritingPayload →
    Step s { s with
      cpp_pc := CppPC.Idle,
      slots := fun i =>
        if i = s.cpp_idx then
          { payload := (s.slots i).payload, seq := (s.slots i).seq + 1 }
        else s.slots i,
      cpp_idx := (s.cpp_idx + 1) % 8
    }
```

含义：

1. 写 payload 完成后，C++ 退出 `WritingPayload`。
2. 当前 slot 的 `seq` 再加 1，从奇数回到偶数。
3. 写指针前移到下一个 ring slot。

源码对应：

- `SDK/c++/coroTracer.h:169-174`

```cpp
slot.seq.store(old_seq + 2, std::memory_order_release);
event_count++;
```

严格来说，源码里推进的是 `event_count`，不是一个显式的 `cpp_idx` 变量。  
但因为选槽位的公式是 `event_count % 8`，所以 Lean 里的

```lean
cpp_idx := (cpp_idx + 1) % 8
```

和源码的 ring 推进语义是等价的。

### 6.4 `go_scan`

位置：`proof/proof.lean:65-72`

Lean 定义：

```lean
| go_scan (s : SystemState) :
    s.go_pc = GoPC.ScanSeq →
    Step s { s with
      go_pc := if (s.slots s.go_scan_idx).seq % 2 = 0 ∧
                  (s.slots s.go_scan_idx).seq > s.go_last_seen s.go_scan_idx
               then GoPC.ReadPayload
               else GoPC.ScanSeq,
      go_observed_seq := (s.slots s.go_scan_idx).seq
    }
```

这一步正对应 Go 读侧第一个阶段：

1. 读取当前 slot 的 `seq`
2. 记录到 `go_observed_seq`
3. 如果它是偶数并且比 `last_seen` 新，就进入 `ReadPayload`
4. 否则留在 `ScanSeq`

源码对应：

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

为什么 Lean 写成单步状态机，而 Go 是一个函数里的几行代码：

- Lean 需要把“读 seq 并决定下一阶段”显式化
- 这样后面的定理才能对 `GoPC` 的不同状态分别做讨论

### 6.5 `go_read`

位置：`proof/proof.lean:75-80`

Lean 定义：

```lean
| go_read (s : SystemState) :
    s.go_pc = GoPC.ReadPayload →
    Step s { s with
      go_pc := GoPC.ValidateSeq,
      go_temp_payload := (s.slots s.go_scan_idx).payload
    }
```

含义：

1. 前一阶段已经确认这个 slot 看起来是“值得读”的。
2. 现在开始真正把 payload 拷到本地临时变量。
3. 读完后，进入 `ValidateSeq`。

源码对应：

- `structure/station.go:64-67`

```go
localTID := slot.TID
localAddr := slot.Addr
localIsActive := slot.IsActive
localTS := slot.Timestamp
```

Lean 里的 `go_temp_payload` 就是对这些局部变量的抽象。

### 6.6 `go_validate_pass`

位置：`proof/proof.lean:83-89`

Lean 定义：

```lean
| go_validate_pass (s : SystemState) :
    s.go_pc = GoPC.ValidateSeq ∧
    (s.slots s.go_scan_idx).seq = s.go_observed_seq →
    Step s { s with
      go_pc := GoPC.ScanSeq,
      go_last_seen := fun i =>
        if i = s.go_scan_idx then s.go_observed_seq else s.go_last_seen i,
      jsonl_log := (s.go_observed_seq, s.go_temp_payload) :: s.jsonl_log
    }
```

含义：

1. Go 已经读完 payload，准备复核。
2. 如果当前共享内存里的 `seq` 仍然等于刚才看到的 `go_observed_seq`，
3. 说明读取 payload 的这段时间里，C++ 没有碰这个 slot。
4. 因此可以安全提交：
   - 更新 `last_seen`
   - 把这条记录加入日志
   - 状态机回到 `ScanSeq`

源码对应：

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

这里最核心的一点是：  
**提交动作发生在第二次 `seq` 验证成功之后，而不是之前。**

### 6.7 `go_validate_fail`

位置：`proof/proof.lean:92-97`

Lean 定义：

```lean
| go_validate_fail (s : SystemState) :
    s.go_pc = GoPC.ValidateSeq ∧
    (s.slots s.go_scan_idx).seq ≠ s.go_observed_seq →
    Step s { s with
      go_pc := GoPC.ScanSeq
    }
```

含义：

1. Go 刚刚把 payload 拷到了本地。
2. 但复核时发现 slot 里的 `seq` 已经变了。
3. 说明这次读取过程中确实发生了并发覆盖或 wrap-around。
4. 那么这份 `go_temp_payload` 必须丢弃，绝对不能提交。

源码对应：

- `structure/station.go:71-76`

```go
seq2 := atomic.LoadUint64(&slot.Seq)
if seq1 != seq2 {
    continue
}
```

注意这里 `continue` 的语义非常关键：  
它意味着 **不更新 `lastSeen`，也不写日志**。  
Lean 里也是同样处理：只把 `go_pc` 退回 `ScanSeq`，别的都不提交。

---

## 7. `SystemInvariant` 在表达什么

位置：`proof/proof.lean:106-110`

定义如下：

```lean
def SystemInvariant (s : SystemState) : Prop :=
  (∀ seq pay, (seq, pay) ∈ s.jsonl_log → seq % 2 = 0) ∧
  (s.go_pc = GoPC.ReadPayload ∨ s.go_pc = GoPC.ValidateSeq →
    s.go_observed_seq % 2 = 0)
```

它由两个条件组成。

### 7.1 条件 A：日志里的所有 `seq` 都必须是偶数

这表示：

- 任何已经被提交的记录
- 都来自某次“写事务完成之后”的稳定版本
- 而不是来自奇数 `seq` 的中间写阶段

这是最终想守住的外部语义保证。

### 7.2 条件 B：只要 Go 已经进入读取或验证阶段，它手上的 `observed_seq` 必须是偶数

这表示：

- Go 不会拿着一个奇数 `seq` 进入 payload 读取阶段
- 因而不会基于“C++ 正在写”的快照继续推进

这相当于给整个读协议加了一道内部约束：

- 日志安全是最终目标
- `observed_seq` 为偶数是实现这个目标所依赖的中间条件

---

## 8. 第一个定理：`system_is_always_safe`

位置：`proof/proof.lean:113-191`

定理原文：

```lean
theorem system_is_always_safe (s1 s2 : SystemState) (h_step : Step s1 s2) :
    SystemInvariant s1 → SystemInvariant s2 := by
```

这个定理的意思是：

- 如果旧状态 `s1` 满足不变量
- 并且 `s1 -> s2` 是一个合法 `Step`
- 那么新状态 `s2` 仍满足不变量

这是一个 **一步保持性证明**。  
它没有直接写成“所有可达状态都安全”，但这已经是那类全局结论的核心砖块。

只要你再加一个“初始状态满足不变量”的结论，并对执行步数做归纳，就能得到整段执行始终安全。

### 8.1 证明结构总览

Lean 里这段证明分成两个大目标：

1. 证明新状态的日志里所有 `seq` 仍是偶数
2. 证明新状态如果处于 `ReadPayload` 或 `ValidateSeq`，那么 `observed_seq` 仍是偶数

对应代码就是：

```lean
constructor
```

之后分别证明两个分量。

### 8.2 目标 A：为什么“新日志里全是偶数 seq”成立

位置：`proof/proof.lean:122-147`

证明思路分两类情况。

#### 情况 1：这一步根本不改日志

对于下面这些 `Step`：

- `cpp_start_write`
- `cpp_write_data`
- `cpp_end_write`
- `go_scan`
- `go_read`
- `go_validate_fail`

Lean 都直接写成：

```lean
exact h_log_even seq pay h_in_s2
```

意思很简单：

- 因为 `jsonl_log` 没变
- 所以新状态里任意一条日志，本来就在旧状态里
- 旧状态已经知道它的 `seq` 是偶数
- 直接继承即可

这和真实源码完全一致，因为：

- C++ 写事务不会直接写 JSONL
- Go 的 `scan` 和 `read` 阶段也不会写 JSONL
- `validate_fail` 明确是丢弃，不落盘

#### 情况 2：唯一会改日志的步骤是 `go_validate_pass`

这一步最关键，对应 `proof/proof.lean:132-147`。

Lean 先把

```lean
(seq, pay) ∈ new_log
```

展开为两种可能：

1. 它就是刚刚新插进去的头元素
2. 它是旧日志里本来就有的元素

也就是：

```lean
simp only [List.mem_cons] at h_in_s2
cases h_in_s2 with
| inl h_eq => ...
| inr h_mem => ...
```

##### 子情况 2.1：它就是新插入的那条记录

Lean 通过：

```lean
injection h_eq with h_seq h_pay
rw [h_seq]
```

把“新插入记录的 `seq`”化简为 `s.go_observed_seq`。

然后它调用旧状态中的不变量条件 B：

```lean
apply h_obs_even
right
exact h_cond.left
```

这里的逻辑链条是：

1. `go_validate_pass` 的前提说明旧状态 `s` 处于 `GoPC.ValidateSeq`
2. 不变量条件 B 说：只要旧状态处于 `ReadPayload` 或 `ValidateSeq`，`go_observed_seq` 就必须是偶数
3. 因此新插入日志的 `seq = go_observed_seq` 也是偶数

这一步特别漂亮，因为它抓住了整个协议的精髓：

- 新日志的安全性，不是凭空来的
- 它依赖于 Go 在更早一步就已经只允许“偶数 seq”的快照进入读取路径

##### 子情况 2.2：它是旧日志里的老元素

这时直接回到旧状态不变量：

```lean
exact h_log_even seq pay h_mem
```

完全自然。

### 8.3 目标 B：为什么“进入 Read/Validate 时 observed_seq 必为偶数”成立

位置：`proof/proof.lean:152-191`

这部分是对 `Step` 再做一次分类讨论。

#### 分支 1：C++ 的三个步骤

Lean 写法：

```lean
case cpp_start_write => apply h_obs_even; exact h_pc_s2
case cpp_write_data => apply h_obs_even; exact h_pc_s2
case cpp_end_write   => apply h_obs_even; exact h_pc_s2
```

原因是：

- 这些步骤只动 C++ 写侧，不动 Go 的 `go_pc` 和 `go_observed_seq`
- 所以新状态若满足“Go 正处于 Read/Validate”，那旧状态也同样满足
- 直接继承旧不变量即可

#### 分支 2：`go_scan`

这是最有意思的一段，对应 `proof/proof.lean:160-172`。

Lean 先展开 `if` 分支：

```lean
dsimp at h_pc_s2 ⊢
split at h_pc_s2
```

然后分两种情况。

##### 情况 2.1：`if` 条件为真

也就是：

- `seq` 是偶数
- 而且 `seq > last_seen`

这时新状态会进入 `ReadPayload`。  
Lean 直接拿到 `h_if.left`，也就是“`seq % 2 = 0`”这一部分，完成证明。

这正对应 Go 源码里：

```go
if seq1 <= lastSeen { continue }
if seq1%2 != 0 { continue }
```

只有通过这两个判断，Go 才会真的开始读 payload。

##### 情况 2.2：`if` 条件为假

这时新状态根本不会进入 `ReadPayload`，会留在 `ScanSeq`。  
可我们的目标前提 `h_pc_s2` 却说“新状态正处于 `ReadPayload` 或 `ValidateSeq`”。  
于是直接矛盾，Lean 用 `contradiction` 收尾。

#### 分支 3：`go_read`

位置：`proof/proof.lean:175-178`

逻辑是：

1. `go_read` 只可能从 `ReadPayload` 进入 `ValidateSeq`
2. 所以只要旧状态处于 `ReadPayload`
3. 旧不变量就已经保证旧状态里的 `go_observed_seq` 为偶数
4. 新状态沿用同一个 `go_observed_seq`
5. 所以结论成立

Lean 写成：

```lean
apply h_obs_even
left
exact h_pc
```

#### 分支 4：`go_validate_pass` 和 `go_validate_fail`

这两个分支都会把 `go_pc` 退回 `ScanSeq`。  
所以如果目标前提说“新状态处于 `ReadPayload` 或 `ValidateSeq`”，那必然是假。

Lean 直接展开后做矛盾消解：

```lean
cases h_pc_s2 with
| inl h1 => contradiction
| inr h2 => contradiction
```

### 8.4 这个定理和真实源码的意义

这个定理不是在证明“Go 一定能读到所有数据”，而是在证明：

- **凡是 Go 真正提交进日志的数据，都符合安全约束。**

也就是说，系统允许“宁可丢掉一次读取机会，也绝不提交一次可能撕裂的记录”。

这与源码设计完全一致：

- 奇数 `seq` 跳过
- 前后 `seq` 不一致跳过
- 只有前后稳定时才 `WriteSafeSlot`

这正是一个典型的“以可丢弃重试换一致性”的 lock-free 读协议。

---

## 9. 第二个定理：`go_obstruction_free_liveness`

位置：`proof/proof.lean:197-250`

定理原文简化后可以读成：

> 如果一开始 Go 在 `ScanSeq`，当前 slot 上已经有一个新的偶数 `seq`，并且从 `s0` 到 `s3` 整个 3 步窗口里 C++ 一直是安静的，那么 Go 最终一定会回到 `ScanSeq`，并且把这条 `(observed_seq, payload)` 插入日志。

### 9.1 这个定理为什么叫 obstruction-free

因为它不是说：

- “无论 C++ 怎么疯狂覆盖，Go 都能马上成功”

而是说：

- “只要没有外部干扰，Go 这条路径就一定能走完”

这正是 obstruction-free 的含义：

- 一旦独占执行窗口，操作就能完成

这和现实完全一致。  
在极高并发下，Go 某次读取可能因为 `seq1 != seq2` 而失败，但只要出现一个很短的空档，它就会成功。

### 9.2 前提条件逐条解释

位置：`proof/proof.lean:198-209`

#### `h_go_pc`

```lean
(h_go_pc : s0.go_pc = GoPC.ScanSeq)
```

表示 Go 一开始正在准备扫描。

这对应 `Harvest` 的起点，也就是读某个 slot 前的第一个阶段。

#### `h_new_data`

```lean
(h_new_data : (s0.slots s0.go_scan_idx).seq > s0.go_last_seen s0.go_scan_idx)
```

表示当前 slot 里确实有新数据，不是旧版本。

对应 Go 源码：

```go
if seq1 <= lastSeenSeqs[i] {
    continue
}
```

如果这个条件不成立，Go 本来就没有理由去读 payload。

#### `h_even`

```lean
(h_even : (s0.slots s0.go_scan_idx).seq % 2 = 0)
```

表示这条新数据是稳定态，不是写到一半的奇数态。

对应 Go 源码：

```go
if seq1%2 != 0 {
    continue
}
```

#### `h_quiet0` 到 `h_quiet3`

```lean
(h_quiet0 : s0.cpp_pc = CppPC.Idle)
(h_quiet1 : s1.cpp_pc = CppPC.Idle)
(h_quiet2 : s2.cpp_pc = CppPC.Idle)
(h_quiet3 : s3.cpp_pc = CppPC.Idle)
```

这组条件表示：

- 在整个三步窗口中，C++ 都没有进入 `WritingPayload`

也就是 Go 拥有了一个短暂但完整的无干扰窗口。

这里可以特别说明一点：

- 当前证明正文实际上主要用到了 `h_quiet0`、`h_quiet1`、`h_quiet2`
- `h_quiet3` 在证明体里没有被关键使用
- 但把它放进定理陈述是合理的，因为它表达了“整个观测窗口都安静”的更强语义

### 9.3 为什么必须是“三步”

因为 Go 完成一次成功提交，逻辑上就需要这三步：

1. `go_scan`
2. `go_read`
3. `go_validate_pass`

这正是 `Harvest` 的三个阶段。

### 9.4 证明是怎么走的

#### 第一步：`step1` 只能是 `go_scan`

位置：`proof/proof.lean:213-223`

Lean 对 `step1` 做分类讨论：

```lean
cases step1
```

然后排除所有不可能分支：

- `cpp_start_write`：和 `h_quiet0` 冲突
- `cpp_write_data`：也和 `h_quiet0` 冲突
- `cpp_end_write`：同样冲突
- `go_read`：和 `h_go_pc = ScanSeq` 冲突
- `go_validate_pass` / `go_validate_fail`：也和 `h_go_pc = ScanSeq` 冲突

所以唯一可能剩下的就是：

```lean
case go_scan h_pc => ...
```

然后 Lean 构造：

```lean
have h_if : seq_even ∧ seq_new := ⟨h_even, h_new_data⟩
```

得出 `go_scan` 中的 `if` 条件必为真，于是：

```lean
have h_s1_pc_is_read : ... = GoPC.ReadPayload := by simp [h_if]
```

这表示第一步之后，Go 一定进入 `ReadPayload`。

#### 第二步：`step2` 只能是 `go_read`

位置：`proof/proof.lean:225-234`

同样，Lean 对 `step2` 分类讨论并排除：

- 任何 C++ 步骤都与 `h_quiet1` 冲突
- `go_scan` 与“当前状态已经是 `ReadPayload`”冲突
- `go_validate_pass` / `go_validate_fail` 也与状态不匹配

所以只剩：

```lean
case go_read h2 =>
```

这意味着 Go 成功完成了 payload 拷贝，并进入 `ValidateSeq`。

#### 第三步：`step3` 只能是 `go_validate_pass`

位置：`proof/proof.lean:236-250`

再一次分类讨论：

- 任何 C++ 步骤都与 `h_quiet2` 冲突
- `go_scan`、`go_read` 与当前状态不匹配

还剩两个候选：

- `go_validate_fail`
- `go_validate_pass`

Lean 接着排除 `go_validate_fail`：

```lean
case go_validate_fail h =>
  have hp := h.right
  exact False.elim (hp rfl)
```

这里的意思是：

- `go_validate_fail` 需要满足“当前 slot 里的 `seq ≠ go_observed_seq`”
- 但在前面两步之间 C++ 没有动过这个 slot
- 所以当前 `seq` 就应该等于之前 `go_scan` 记录下来的 `go_observed_seq`
- 因而“`≠`”会直接矛盾

所以只能是：

```lean
case go_validate_pass _ =>
  constructor
  · rfl
  · simp [List.mem_cons]
```

这就得到最终结论：

1. `s3.go_pc = GoPC.ScanSeq`
2. `(s3.go_observed_seq, s3.go_temp_payload) ∈ s3.jsonl_log`

也就是：Go 成功提交了这条记录。

### 9.5 这个定理在真实代码中的意义

这一定理告诉我们：

- Go 读侧并不是“运气好才读到数据”
- 只要写侧在关键窗口内不改这个 slot
- Go 的三阶段协议就一定能完成一次成功提交

这正对应 `Harvest` 的现实语义：

1. `seq1` 读到的是新的偶数值
2. 复制字段
3. `seq2` 仍等于 `seq1`
4. `WriteSafeSlot`

---

## 10. Lean 与 Go/C++ 的逐段映射总表

下面这张表适合在读源码时来回对照。

| Lean 位置 | Lean 含义 | Go/C++ 源码对应 |
| --- | --- | --- |
| `proof/proof.lean:41-46` | 开始写：`seq` 变奇数 | `SDK/c++/coroTracer.h:157-159` |
| `proof/proof.lean:49-53` | 写 payload | `SDK/c++/coroTracer.h:164-167` |
| `proof/proof.lean:56-62` | 结束写：`seq` 变偶数并推进 ring | `SDK/c++/coroTracer.h:169-174` |
| `proof/proof.lean:65-72` | Go 扫描 `seq` 并决定是否进入读取 | `structure/station.go:50-58` |
| `proof/proof.lean:75-80` | Go 拷贝 payload 到局部变量 | `structure/station.go:64-67` |
| `proof/proof.lean:83-89` | Go 验证成功并提交日志 | `structure/station.go:71-84` + `structure/jsonl.go:73-76` |
| `proof/proof.lean:92-97` | Go 验证失败并丢弃读取结果 | `structure/station.go:71-76` |
| `proof/proof.lean:106-110` | 安全不变量 | 整体约束，分散体现于 `Harvest` 的判定逻辑 |
| `proof/proof.lean:113-191` | 安全性保持证明 | 解释为什么 `Harvest` 的控制流不会写入脏数据 |
| `proof/proof.lean:197-250` | 无干扰窗口下的活性证明 | 解释为什么 `Harvest` 在稳定窗口中一定能成功提交 |

---

## 11. 一次完整事件如何从 C++ 流到 Go 日志

这一节把证明和真实执行串起来。

### 11.1 协程挂起时，C++ 触发写入

在 `SDK/c++/coroTracer.h:191-193`：

```cpp
tracer->write_trace(reinterpret_cast<uint64_t>(h.address()), false);
```

这表示：

- 当前协程即将挂起
- 把句柄地址和 `is_active = false` 写进当前 station 的 ring slot

恢复时则在 `SDK/c++/coroTracer.h:197-199`：

```cpp
tracer->write_trace(0, true);
```

表示：

- 协程重新变成活跃态

### 11.2 C++ 在共享内存里完成一次完整写事务

`write_trace` 里会：

1. 选中 `event_count % 8` 对应的 slot
2. 把 `seq` 设成奇数
3. 写入字段
4. 把 `seq` 设成偶数
5. `event_count++`

在 Lean 里，这正对应：

1. `cpp_start_write`
2. `cpp_write_data`
3. `cpp_end_write`

### 11.3 如果 Go 正在睡眠，C++ 尝试唤醒它

在 `SDK/c++/coroTracer.h:176-180`：

```cpp
if (g_header->tracer_sleeping.load(std::memory_order_acquire) == 1) {
    uint32_t expected = 1;
    if (g_header->tracer_sleeping.compare_exchange_strong(expected, 0, std::memory_order_acq_rel)) {
        trigger_uds_wakeup();
    }
}
```

这部分是工程层面的“快醒机制”。  
它很重要，但不属于当前 Lean 证明覆盖的范围。

### 11.4 Go 引擎扫描 station

在 `engine/engine.go:112-123`：

```go
allocated := atomic.LoadUint32(&e.header.AllocatedCount)
for i := uint32(0); i < allocated; i++ {
    totalHarvested += e.stations[i].Harvest(&e.lastSeen[i], e.writer)
}
```

这说明真实系统会遍历多个 station，而 Lean 模型只关注其中一个当前 slot。  
这是一个典型的“局部性质推广到整体循环”的关系：

- 只要每个 slot 的读取协议安全
- 外层循环逐个调用它就不会破坏这个安全性

### 11.5 Go 在 `Harvest` 中执行三阶段协议

这部分就是 Lean 的 `GoPC` 三态：

1. `ScanSeq`
2. `ReadPayload`
3. `ValidateSeq`

成功则对应 `go_validate_pass`，失败则对应 `go_validate_fail`。

### 11.6 成功提交后写入 JSONL

最终数据会在 `structure/jsonl.go` 中拼成：

```json
{"probe_id":...,"tid":...,"addr":"0x...","seq":...,"is_active":...,"ts":...}
```

Lean 不关心 JSON 文本格式，但关心：

- 进入日志的这条记录，是不是来自一个经验证的稳定快照

而前面的两个定理正是在守住这个性质。

---

## 12. 这份证明依赖哪些现实前提

形式化证明很强，但它不是魔法。它依赖源码实现满足几个关键前提。

### 12.1 C++ 必须真的遵守“先奇数、后 payload、再偶数”的顺序

这在源码中通过 `std::memory_order_release` 来表达。

对应位置：

- `SDK/c++/coroTracer.h:159`
- `SDK/c++/coroTracer.h:172`

Lean 本身没有逐条模拟 CPU 内存序模型，但它的 `Step` 假设正是建立在这个协议上。

换句话说，Lean 证明的是：

- **只要实现满足这个写序约束，那么不会提交脏数据。**

### 12.2 Go 读 `seq` 必须用原子读

对应位置：

- `structure/station.go:50`
- `structure/station.go:71`

```go
seq1 := atomic.LoadUint64(&slot.Seq)
seq2 := atomic.LoadUint64(&slot.Seq)
```

这是 Go 侧观察一致性的基础。  
Lean 里虽然不直接写“Acquire”，但 `go_scan` 与 `go_validate_*` 的语义默认了这个原子观察点。

### 12.3 `lastSeen` 必须是按 slot 维护，而不是按 station 只记一个值

真实源码中：

- `TracerEngine.lastSeen` 的类型是 `[][8]uint64`
- 每个 station 下有 8 个独立 slot 的最近已提交 `seq`

对应位置：

- `engine/engine.go:33`
- `engine/engine.go:89`
- `structure/station.go:43`

这和 Lean 里的：

```lean
go_last_seen : Nat → Nat
```

在单个 station/单个扫描索引的抽象下是对应的。

### 12.4 环形复用必须和 `seq` 增长配套

因为 slot 会被反复复用，所以单看“读到数据”是不够的，必须知道它是不是同一代数据。  
这就是 `seq` 的意义。

真实源码里：

- ring 位置来自 `event_count % 8`
- 代际区分来自每个 slot 自己的 `seq`

Lean 里则把它抽象为：

- `cpp_idx := (cpp_idx + 1) % 8`
- 当前 slot 的 `seq` 按写事务增长

---

## 13. 这份证明没有覆盖什么

这一节同样重要，因为它告诉我们“证明边界”在哪里。

### 13.1 没有证明跨语言 ABI 布局一定正确

虽然源码里已经非常明确地做了布局约束：

- Go：`GlobalHeader` / `StationData` / `Epoch`
- C++：`alignas(1024)` / `alignas(64)`
- 文档：`docs/cTP.md`

但 Lean 文件并没有证明：

- Go 结构体大小一定与 C++ 完全一致
- 编译器不会引入意外 padding

这部分靠的是工程实现和人工约束，而不是 Lean 中的定理。

### 13.2 没有证明 `InitTracer()`、mmap、UDS 一定成功

Lean 没有建模：

- `open`
- `mmap`
- `connect`
- `listen`
- `Accept`
- 子进程启动

也就是说，它证明的是“协议正确”，不是“部署环境永不失败”。

### 13.3 没有形式化 `TracerSleeping` / UDS 唤醒

`engine/engine.go:126-162` 和 `SDK/c++/coroTracer.h:176-180` 实现了非常实用的休眠/唤醒机制。  
但当前 Lean 证明没有覆盖这部分。

所以它不能直接推出：

- “Go 引擎一定会被及时唤醒”
- “不会发生任何 wakeup 丢失”

它能推出的是：

- **一旦 Go 真正进入扫描并拿到一个稳定窗口，提交的数据不会脏；若窗口足够安静，则这次提交一定成功。**

### 13.4 没有直接证明“所有执行路径上的全局安全”

严格说，`system_is_always_safe` 是一步保持性定理，不是“可达状态归纳闭包”的完整终局定理。  
不过它已经给出了最核心的归纳步。

要把它扩成更完整的叙述，通常还需要：

1. 一个初始状态满足 `SystemInvariant` 的 lemma
2. 一个关于多步执行的归纳 theorem

当前文件还没有把这两层完全展开。

---

## 14. 为什么这份证明足够有说服力

因为它抓住了最危险、最难凭肉眼完全确认的一层：

- 并发写和并发读交错时
- Go 会不会把“看起来像真数据、实际是拼出来的假数据”写进日志

只要这件事没有被形式化，任何“我们觉得应该没问题”的说法都不牢靠。  
而 `proof/proof.lean` 恰恰把这件事拆成了：

1. 明确的状态机
2. 明确的允许步骤
3. 明确的不变量
4. 对所有步骤的穷举讨论

这让结论不再依赖直觉，而依赖可检查的推导。

---

## 15. 一个很实用的阅读顺序

如果你想把 Lean 和源码真正对起来，推荐这样读。

### 第一步：先读真实读写协议

先看：

- `SDK/c++/coroTracer.h:149-182`
- `structure/station.go:42-87`

目标是先建立一个直觉：

- 写侧如何把 `seq` 变奇再变偶
- 读侧如何两次读 `seq`

### 第二步：再看 Lean 的 `Step`

再看：

- `proof/proof.lean:38-97`

你会发现它几乎就是把刚才的协议翻译成了状态机语言。

### 第三步：读安全性定理

看：

- `proof/proof.lean:106-191`

重点关注：

- 为什么只有 `go_validate_pass` 能改日志
- 为什么新插入的日志项一定继承了“偶数 observed_seq”

### 第四步：读活性定理

看：

- `proof/proof.lean:197-250`

重点关注：

- 为什么三步窗口足够
- 为什么无干扰时 `go_validate_fail` 不可能发生

---

## 16. 最后的总结

如果把这份证明压缩成一句话，那就是：

> coroTracer 不是靠“希望 Go 刚好没读到一半”来保证日志正确，而是通过一个显式的奇偶 `seq` 协议，再配合 Go 侧的“双读验证”，并且已经把这套机制抽象成 Lean 状态机，证明了提交到日志的数据不会来自半写入状态；同时也证明了只要写侧短暂停止干扰，Go 侧一定能完成一次成功提交。

再展开一点，可以分成三层理解：

1. **C++ 写协议**  
   先奇数、再写 payload、再偶数。

2. **Go 读协议**  
   先看 `seq` 是否新且稳定，再复制 payload，最后复核 `seq` 是否没变。

3. **Lean 证明**  
   安全性定理保证“不会提交脏数据”，活性定理保证“在安静窗口中一定能提交成功”。

这三层正好分别落在：

- `SDK/c++/coroTracer.h`
- `structure/station.go` / `structure/jsonl.go`
- `proof/proof.lean`

也就是说，这份 Lean 证明不是和工程代码分离的“理论附件”，而是对项目最核心并发协议的形式化注释。
