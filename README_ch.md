# coroTracer：跨语言、零拷贝的协程观测工具

![Go Engine](https://img.shields.io/badge/Engine-Go_1.21+-00ADD8.svg)
![SDK C++](https://img.shields.io/badge/SDK-C++20-blue.svg)
![Arch](https://img.shields.io/badge/Arch-Language_Agnostic-orange.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)

![UDSWakeupMechanics.gif](source/UDSWakeupMechanics.gif)

> **开发初衷**：我之前在调一个自己的 M:N 调度器时，遇到过一个非常恶心的问题。高并发下系统吞吐量会突然掉到零，但 ASAN 和 TSAN 都是绿的，因为它根本不是传统意义上的内存破坏，而是一次典型的 `lost wakeup`。协程逻辑上已经永远等不回来了，可常规工具又很难把这种“状态机断裂”直接抓出来。coroTracer 就是为这种问题写的。

coroTracer 是一个 **进程外（out-of-process）** 的协程采集器。  
它专门面向 M:N 协程调度器，目标很明确：

- 抓协程状态切换
- 降低对目标进程的干扰
- 输出可复用的原始轨迹
- 让后续分析、落库、离线处理都建立在可靠的底层采集之上

它现在的定位不是 APM，也不是在线分析平台。  
当前仓库专注于两件事：

1. **把协程状态安全地采集成 JSONL**
2. **把已有 JSONL 导出成 SQLite / MySQL / PostgreSQL / CSV**

另外，采集协议的核心安全性已经在 Lean 4 里建模并证明，相关文件见：

- [proof/proof.lean](./proof/proof.lean)
- [proof.md](./proof/proof.md)
- [proof_en.md](./proof/proof_en.md)
- [docs/cli_usage_ch.md](./docs/cli_usage_ch.md)
- [docs/cli_usage.md](./docs/cli_usage.md)

> **项目现状说明**：到目前为止，这个项目已经是闭环可用的。采集、落盘、导出这一整套链路已经能实际工作。真要说当前还比较明显的不足，主要就是采集容量仍然是**有限协程数量**，而不是动态扩容。除此之外，项目本身已经可以使用。后续更新会继续有，但节奏大概率会明显放缓，可能会比之前慢很多。
> 这次更新主要集中在数据格式转换和导出链路上，没有去碰核心采集代码。Codex 确实大幅提高了这次迭代的效率，也让这轮更新能够更快上线。

---

## 架构

```text
+-----------------------+                               +-----------------------+
|   Target Application  |                               |    Go Tracer Engine   |
|  (C++, Rust, Zig...)  |                               |                       |
|                       |       [ Lock-Free SHM ]       |                       |
|  +-----------------+  |      +-----------------+      |  +-----------------+  |
|  |  cTP SDK Probe  |=======> | StationData [N] | <=======|  Harvester Loop   |  |
|  +-----------------+  |  Write +-----------------+ Read |  +-----------------+  |
|                       |               ^               |                       |
|       [ Socket ]      |---(Wakeup)---UDS---(Listen)---|      [ File I/O ]     |
+-----------------------+                               +-----------------------+
                                                                        |
                                                                        v
                                                               +------------------+
                                                               |  trace_output    |
                                                               |     .jsonl       |
                                                               +------------------+
                                                                        |
                                                                        v
                                                          +----------------------------------+
                                                          | SQLite / MySQL / PostgreSQL / CSV |
                                                          +----------------------------------+
```

---

## 当前能力

### 1. 采集模式

Go 引擎负责：

- 创建共享内存
- 创建 Unix Domain Socket
- 拉起目标程序
- 持续扫描共享内存里的协程事件
- 以 JSONL 形式落盘

输出的 JSONL 每行大致长这样：

```json
{"probe_id":123,"tid":456,"addr":"0x0000000000000000","seq":2,"is_active":true,"ts":123456789}
```

字段含义对应源码里的 `TraceRecord`：

- `probe_id`：协程探针唯一标识
- `tid`：真实线程 ID
- `addr`：挂起点地址或相关协程地址
- `seq`：槽位序列号
- `is_active`：当前是否处于活跃态
- `ts`：时间戳

### 2. 导出模式

仓库现在内置了 `export/` 目录，支持把 **已有 JSONL** 转成：

- SQLite 数据库
- MySQL 数据库
- PostgreSQL 数据库
- DataFrame 友好的 CSV 文件

注意这里是 **已有 JSONL 的二次导出**，不是“边采集边转数据库”。

### 3. SDK

当前提供了一个 C++20 头文件版 SDK：

- [SDK/c++/coroTracer.h](SDK/c++/coroTracer.h)

同时也提供了一个不依赖框架、基于 Rust poll 模型的 SDK：

- [SDK/rust](SDK/rust)

它们的职责是：

- 连接共享内存
- 连接 UDS
- 在协程挂起 / 恢复时写入状态
- 遵守 cTP 的内存契约

---

## 核心机制

这套设计的核心思想很简单：

> **把执行平面和观测平面彻底拆开。**

目标进程只负责把状态写进共享内存。  
Go 采集器在进程外做异步收割，不把复杂逻辑塞回目标进程里。

### 1. 共享内存协议（cTP）

底层协议文档见：

- [docs/cTP.md](docs/cTP.md)

核心点有三个：

1. `GlobalHeader` 和 `StationData` 都强制按固定大小布局
2. `Epoch` 强制对齐到 64 字节 cache line
3. 读写双方通过 `seq` 实现无锁一致性协议

### 2. C++ 写侧协议

写侧并不是“直接把字段一把写进去”，而是遵守一个很明确的顺序：

1. 先把 `seq` 改成奇数，表示“正在写”
2. 再写 payload
3. 最后把 `seq` 改成偶数，表示“写完了”

这对应 [SDK/c++/coroTracer.h](SDK/c++/coroTracer.h) 里的 `PromiseMixin::write_trace`。

### 3. Go 读侧协议

Go 读侧也不是“看见数据就信”，而是走三步：

1. 先读一次 `seq`
2. 如果 `seq` 是偶数且比本地 `lastSeen` 新，才去拷 payload
3. 拷完以后再读一次 `seq`
4. 只有两次 `seq` 一样，才真正写 JSONL

这部分在：

- [structure/station.go](structure/station.go)
- [structure/jsonl.go](structure/jsonl.go)

### 4. UDS 智能唤醒

为了避免 Go 引擎在低流量时一直空转：

- Go 空闲时会把 `TracerSleeping` 设成 `1`
- C++ 写入完成后如果发现引擎在睡眠，就发一个 1 字节 UDS 唤醒信号

这样高吞吐时避免系统调用风暴，低吞吐时又不会纯忙等。

---

## 快速开始

### 1. 编译

```bash
go build -o coroTracer main.go
```

### 2. 采集一个目标程序

```bash
./coroTracer -n 256 -cmd "./your_target_app" -out trace.jsonl
```

这条命令做的事情是：

- 预分配 256 个 station
- 拉起 `./your_target_app`
- 把轨迹写到 `trace.jsonl`

这里有一个重要约束：

- `-cmd` 模式只负责采集 JSONL
- 不会在同一轮运行里继续导出数据库

也就是说，采集和导出是 **两个独立阶段**。

### 3. 接入 C++ SDK

目标程序会自动从环境变量继承 IPC 配置。

最小接入大概是这样：

```cpp
#include "coroTracer.h"

int main() {
    corotracer::InitTracer();
    // ... 启动你的调度器
}
```

如果是 coroutine promise，可以继承 `PromiseMixin`：

```cpp
struct promise_type : public corotracer::PromiseMixin {
    // 你的业务逻辑
};
```

SDK 会在内部记录 `await_suspend` / `await_resume` 对应的状态切换。

---

## 导出 JSONL

导出模式只处理 **已经存在的 JSONL 文件**。  
不能和 `-cmd` 同时使用。

也就是说，下面这种是允许的：

```bash
./coroTracer -export sqlite -in trace.jsonl
```

但下面这种是不允许的：

```bash
./coroTracer -cmd "./your_target_app" -export sqlite
```

### 1. 导出 SQLite

```bash
./coroTracer -export sqlite -in trace.jsonl -sqlite-out trace.sqlite
```

说明：

- 默认会把输出文件名推导成 `<input>.sqlite`
- 运行时依赖本机 `sqlite3` 命令

### 2. 导出 CSV（DataFrame 友好格式）

```bash
./coroTracer -export csv -in trace.jsonl -csv-out trace.csv
```

这个 CSV 可以直接喂给：

- pandas
- polars
- DuckDB
- R

### 3. 导出 MySQL

```bash
./coroTracer \
  -export mysql \
  -in trace.jsonl \
  -db-host 127.0.0.1 \
  -db-port 3306 \
  -db-user root \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events
```

如果你用 Unix Socket，也可以传：

```bash
./coroTracer \
  -export mysql \
  -in trace.jsonl \
  -db-user root \
  -db-password your_password \
  -mysql-socket /tmp/mysql.sock
```

说明：

- 运行时依赖本机 `mysql` 命令
- 会自动建库、建表并插入数据

### 4. 导出 PostgreSQL

```bash
./coroTracer \
  -export postgresql \
  -in trace.jsonl \
  -db-host 127.0.0.1 \
  -db-port 5432 \
  -db-user postgres \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events \
  -pg-sslmode disable
```

说明：

- 运行时依赖本机 `psql` 命令
- 会自动检查目标数据库，不存在时尝试创建
- 默认用 `postgres` 作为 maintenance database，可以通过 `-pg-maintenance-db` 改掉

### 5. 常用导出参数

当前支持的导出相关参数包括：

- `-export`
- `-in`
- `-sqlite-out`
- `-csv-out`
- `-db-cli`
- `-db-host`
- `-db-port`
- `-db-user`
- `-db-password`
- `-db-name`
- `-db-table`
- `-mysql-socket`
- `-pg-maintenance-db`
- `-pg-sslmode`

其中：

- `-db-password` 就是直接传用户自己的数据库密码
- `-db-cli` 用来覆盖默认 CLI 名称
  - MySQL 默认是 `mysql`
  - PostgreSQL 默认是 `psql`

更完整的参数说明见：

- [docs/cli_usage_ch.md](docs/cli_usage_ch.md)

---

## Lean 4 证明

这个项目里比较重要的一点是：  
采集协议不是“拍脑袋觉得没问题”，而是已经做了形式化建模。

你可以按这个顺序看：

1. [proof/proof.lean](./proof/proof.lean)
2. [proof.md](./proof/proof.md)
3. [proof_en.md](./proof/proof_en.md)

证明覆盖的核心内容是：

- Go 不会把半写入的脏数据提交进日志
- 在写侧短暂不干扰的窗口里，Go 一定能成功完成一次采集提交

它和源码的对应关系主要在：

- [SDK/c++/coroTracer.h](SDK/c++/coroTracer.h)
- [structure/station.go](structure/station.go)
- [structure/jsonl.go](structure/jsonl.go)

---

## 当前边界

为了避免误解，这里把项目边界写清楚。

### 1. 这个仓库当前不是分析平台

它现在提供的是：

- 底层采集
- JSONL 落盘
- 导出到数据库 / CSV

它不再包含以前那种“内置报告生成器 / 页面分析器”的路线。

### 2. 这个仓库当前重点是 C++20 / Rust SDK

虽然协议本身是语言无关的，但当前仓库里正式给出的 SDK 包括：

- C++20 coroutine 接入
- Rust `Future::poll` 接入

Zig、C 理论上也都能做，因为底层依赖的是：

- `mmap`
- 固定 ABI 布局
- 原子读写契约

### 3. 运行时外部依赖

如果你要用导出功能，当前实现会依赖本机命令行工具：

- SQLite：`sqlite3`
- MySQL：`mysql`
- PostgreSQL：`psql`

这是为了保持项目本身的 Go 依赖尽量轻，不额外引入数据库 driver。

---

## 仓库结构

当前比较关键的目录和文件：

- [main.go](main.go)：程序入口，负责区分采集模式和导出模式
- [engine/engine.go](engine/engine.go)：共享内存、UDS、热循环采集
- [structure/station.go](structure/station.go)：核心读取协议
- [structure/jsonl.go](structure/jsonl.go)：JSONL 落盘
- [export/](export/)：SQLite / MySQL / PostgreSQL / CSV 导出
- [SDK/c++/coroTracer.h](SDK/c++/coroTracer.h)：C++20 SDK
- [SDK/rust/](SDK/rust/)：Rust poll 模型 SDK
- [docs/cTP.md](docs/cTP.md)：内存协议文档
- [proof/proof.lean](./proof/proof.lean)：Lean 4 证明
- [proof.md](./proof/proof.md)：中文证明详解
- [proof_en.md](./proof/proof_en.md)：英文证明详解

---

## 联系方式

> lixia.chat@outlook.com
