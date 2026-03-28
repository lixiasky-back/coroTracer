# coroTracer 参数使用详解

本文档详细解释 `coroTracer` 当前支持的命令行参数，包括：

- 参数作用
- 默认值
- 适用模式
- 常见组合方式
- 容易混淆的点

入口代码：

- [main.go](../main.go)

---

## 1. 两种运行模式

`coroTracer` 当前只有两种模式，而且是**严格互斥**的。

### 采集模式

通过 `-cmd` 启动。

这个模式会：

- 创建共享内存
- 创建 Unix Domain Socket
- 拉起目标程序
- 采集协程事件
- 输出 JSONL

最小示例：

```bash
./coroTracer -cmd "./your_target_app"
```

### 导出模式

通过 `-export` 启动。

这个模式会：

- 读取已经存在的 JSONL
- 转换成 SQLite / MySQL / PostgreSQL / CSV

最小示例：

```bash
./coroTracer -export sqlite -in trace.jsonl
```

### 互斥规则

下面这种组合是**不允许**的：

```bash
./coroTracer -cmd "./your_target_app" -export sqlite
```

当前设计是：

- `-cmd` 负责采集
- `-export` 负责二次导出

它们不是一条命令里的串联流程，而是两个独立阶段。

---

## 2. 参数总表

| 参数 | 默认值 | 模式 | 作用 |
| --- | --- | --- | --- |
| `-n` | `128` | 采集 | 预分配 station 数量 |
| `-cmd` | 空 | 采集 | 要启动并被采集的目标命令 |
| `-shm` | `/tmp/corotracer.shm` | 采集 | 共享内存文件路径 |
| `-sock` | `/tmp/corotracer.sock` | 采集 | UDS 路径 |
| `-out` | `trace_output.jsonl` | 采集 | JSONL 输出路径 |
| `-export` | 空 | 导出 | 导出目标类型 |
| `-in` | 空 | 导出 | 导出模式的输入 JSONL 路径，默认退回到 `-out` |
| `-sqlite-out` | 空 | 导出 | SQLite 输出路径，默认 `<input>.sqlite` |
| `-csv-out` | 空 | 导出 | CSV 输出路径，默认 `<input>.csv` |
| `-db-cli` | 空 | 导出 | 覆盖默认数据库 CLI 名称 |
| `-db-host` | `127.0.0.1` | 导出 | MySQL / PostgreSQL 主机 |
| `-db-port` | `0` | 导出 | MySQL / PostgreSQL 端口，按类型推导默认值 |
| `-db-user` | 空 | 导出 | 数据库用户名 |
| `-db-password` | 空 | 导出 | 数据库密码 |
| `-db-name` | `coro_tracer` | 导出 | 数据库名 |
| `-db-table` | `coro_trace_events` | 导出 | 表名 |
| `-mysql-socket` | 空 | 导出 | MySQL Unix Socket 路径 |
| `-pg-maintenance-db` | `postgres` | 导出 | PostgreSQL 创建数据库时使用的 maintenance database |
| `-pg-sslmode` | 空 | 导出 | 通过 `PGSSLMODE` 传给 PostgreSQL |

---

## 3. 采集模式参数

### `-n`

默认值：

```text
128
```

作用：

- 指定预分配多少个 station
- 可以理解为当前一次运行里的协程采集容量上限

补充：

- 当前项目一个已知限制，就是容量仍然是**固定有限数量**
- 它不是动态扩容的
- 如果你的协程数量明显更多，应该主动调大这个值

示例：

```bash
./coroTracer -n 512 -cmd "./your_target_app"
```

### `-cmd`

默认值：

```text
空
```

作用：

- 指定被 `coroTracer` 启动并采集的目标命令

实现细节：

- 内部用的是 `sh -c`
- 所以带参数的命令也可以直接传

示例：

```bash
./coroTracer -cmd "./server --threads 4 --config ./conf.yaml"
```

注意：

- 只要给了 `-cmd`，就进入采集模式
- 此时不能再给 `-export`

### `-shm`

默认值：

```text
/tmp/corotracer.shm
```

作用：

- 指定共享内存文件路径

适合修改它的场景：

- 多实例并行测试
- 默认路径冲突
- 你想把 IPC 文件放到自定义目录

示例：

```bash
./coroTracer -cmd "./your_target_app" -shm /tmp/case1.shm
```

### `-sock`

默认值：

```text
/tmp/corotracer.sock
```

作用：

- 指定 UDS 唤醒路径

适合修改它的场景：

- 多实例并行测试
- 默认路径冲突

示例：

```bash
./coroTracer -cmd "./your_target_app" -sock /tmp/case1.sock
```

### `-out`

默认值：

```text
trace_output.jsonl
```

作用：

- 指定采集 JSONL 输出文件

示例：

```bash
./coroTracer -cmd "./your_target_app" -out traces/run1.jsonl
```

补充：

- 在纯导出模式下，如果不传 `-in`，程序会退回使用 `-out` 的值作为输入 JSONL 路径

---

## 4. 导出模式参数

### `-export`

默认值：

```text
空
```

作用：

- 指定导出类型

当前支持：

- `sqlite`
- `mysql`
- `postgres`
- `postgresql`
- `dataframe`
- `csv`

说明：

- `postgres` 和 `postgresql` 等价
- `dataframe` 和 `csv` 等价，都会导出 CSV

### `-in`

默认值：

```text
空
```

作用：

- 指定导出模式的输入 JSONL 文件

默认行为：

- 如果不传 `-in`，程序会回退到 `-out`

例如：

```bash
./coroTracer -export sqlite -out trace.jsonl
```

等价于：

```bash
./coroTracer -export sqlite -in trace.jsonl
```

实际使用里更推荐显式传 `-in`。

---

## 5. SQLite 导出参数

### `-sqlite-out`

默认值：

```text
空
```

作用：

- 指定 SQLite 数据库输出路径

默认行为：

- 不传时自动推导成 `<input>.sqlite`

示例：

```bash
./coroTracer -export sqlite -in trace.jsonl -sqlite-out out/trace.sqlite
```

运行时依赖：

- 本机需要有 `sqlite3`

---

## 6. CSV / DataFrame 导出参数

### `-csv-out`

默认值：

```text
空
```

作用：

- 指定 CSV 输出路径

默认行为：

- 不传时自动推导成 `<input>.csv`

示例：

```bash
./coroTracer -export csv -in trace.jsonl -csv-out out/trace.csv
```

适用下游：

- pandas
- polars
- DuckDB
- R

---

## 7. MySQL / PostgreSQL 通用参数

### `-db-cli`

作用：

- 覆盖默认数据库 CLI 名称

默认行为：

- MySQL 默认使用 `mysql`
- PostgreSQL 默认使用 `psql`

### `-db-host`

默认值：

```text
127.0.0.1
```

作用：

- 指定 MySQL / PostgreSQL 主机

### `-db-port`

默认值：

```text
0
```

实际默认行为：

- MySQL 导出时 `0` 会自动变成 `3306`
- PostgreSQL 导出时 `0` 会自动变成 `5432`

### `-db-user`

作用：

- 指定数据库用户名

### `-db-password`

作用：

- 指定数据库密码

说明：

- 这是直接传用户自己的数据库密码
- MySQL 通过 `MYSQL_PWD`
- PostgreSQL 通过 `PGPASSWORD`

安全提示：

- 明文密码可能出现在 shell history 里

### `-db-name`

默认值：

```text
coro_tracer
```

作用：

- 指定数据库名

### `-db-table`

默认值：

```text
coro_trace_events
```

作用：

- 指定表名

---

## 8. MySQL 专用参数

### `-mysql-socket`

默认值：

```text
空
```

作用：

- 通过 Unix Socket 连接 MySQL

一旦设置：

- `-db-host` 和 `-db-port` 会被忽略

示例：

```bash
./coroTracer \
  -export mysql \
  -in trace.jsonl \
  -db-user root \
  -db-password your_password \
  -mysql-socket /tmp/mysql.sock
```

运行时依赖：

- 本机需要有 `mysql`

---

## 9. PostgreSQL 专用参数

### `-pg-maintenance-db`

默认值：

```text
postgres
```

作用：

- 当目标数据库不存在时，程序会先连接这个 maintenance database
- 再尝试执行 `CREATE DATABASE`

### `-pg-sslmode`

默认值：

```text
空
```

作用：

- 通过 `PGSSLMODE` 传给 `psql`

常见值：

- `disable`
- `require`
- `verify-ca`
- `verify-full`

示例：

```bash
./coroTracer \
  -export postgresql \
  -in trace.jsonl \
  -db-user postgres \
  -db-password your_password \
  -pg-sslmode disable
```

运行时依赖：

- 本机需要有 `psql`

---

## 10. 常见命令组合

### 最小采集

```bash
./coroTracer -cmd "./your_target_app"
```

### 指定容量和输出路径采集

```bash
./coroTracer -n 512 -cmd "./your_target_app --threads 4" -out traces/run1.jsonl
```

### 导出 SQLite

```bash
./coroTracer -export sqlite -in traces/run1.jsonl
```

### 导出 CSV

```bash
./coroTracer -export csv -in traces/run1.jsonl
```

### 导出 MySQL

```bash
./coroTracer \
  -export mysql \
  -in traces/run1.jsonl \
  -db-host 127.0.0.1 \
  -db-port 3306 \
  -db-user root \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events
```

### 导出 PostgreSQL

```bash
./coroTracer \
  -export postgresql \
  -in traces/run1.jsonl \
  -db-host 127.0.0.1 \
  -db-port 5432 \
  -db-user postgres \
  -db-password your_password \
  -db-name coro_tracer \
  -db-table coro_trace_events \
  -pg-sslmode disable
```

---

## 11. 常见误区

### `-cmd` 和 `-export` 可以一起用吗？

不可以。它们是互斥模式。

### `-n` 是线程数吗？

不是。它更接近“协程采集容量上限”。

### 不传 `-in` 会怎样？

导出模式下会退回使用 `-out` 的值。

### 为什么数据库导出依赖系统命令，而不是直接用 Go driver？

这是当前实现的取舍。目标是尽量保持仓库 Go 依赖轻量。

---

## 12. 相关文档

- [README_ch.md](../README_ch.md)
- [README.md](../README.md)
- [docs/cTP.md](cTP.md)
- [proof.md](../proof.md)
- [proof_en.md](../proof_en.md)
