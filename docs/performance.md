# 性能基准与调优指南

> 文中数字均为本机实测；受网络与代理影响，只当作量级参考，不要当保底承诺。

## 1. 实测吞吐

### 单连接架构（接入连接池之前）

样本 `t.me/durov/552`（27.71 MiB），直连：

| threads | 平均 | getFile | FLOOD_WAIT |
|---|---|---|---|
| 1 | 0.33 MiB/s | 56 | 0 |
| 2 | 0.47 MiB/s | 56 | 0 |
| 4 | 0.46 MiB/s | 56 | 0 |
| 8 | 0.51 MiB/s | 56 | 0 |

结论：**单 TCP 连接存在服务端限速**，threads 加到 8 也只有 0.51 MiB/s——加线程无收益。
这就是接入 `dcpool` 多连接池的直接动因。

### 多连接池架构（当前版本）

真实端到端验收（`t.me/RAW8K/13179`，7 文件相册共 **10.5 GiB**，走本地 HTTP 代理）：

| 项 | 数值 |
|---|---|
| 总字节 | 10,754 MiB |
| 耗时 | 6275 s（约 1h44m） |
| **平均吞吐** | **≈ 1.71 MiB/s**（threads=4, file_concurrency=2） |
| FLOOD_WAIT | 0 次 |
| 断点续传 | 取消后 `.part`/`.state` 精确保留，重发链接从缺失分片继续 |

注意：该数字是**经本地代理**的值；代理软件自身吞吐是额外变量。直连环境下的多连接池吞吐
尚未实测，预期显著高于代理路径。

## 2. 已固化的默认值与依据

| 参数 | 默认 | 依据 |
|---|---|---|
| `download.threads` | 4 | gotd 官方示例的常用取值；实际线程数还会按文件大小自适应（`bestThreads`：<1MiB→1、<5MiB→2、<20MiB→4、<50MiB→8、≥50MiB→max） |
| `download.file_concurrency` | 2 | 两文件并行时互不饿死；调大受单账号总限速约束 |
| `bot.max_concurrency` | 3 | 并发任务槽位；已测 2 槽 + 1 排队行为正确 |
| chunk 大小 | 1 MiB | Telegram 服务端硬上限；512KiB 实测比 1MiB 慢约 40%（0.70 vs 1.14 MiB/s） |
| 进度节流 | 3 秒 | 按**时间**节流而非按行数；文件完成数变化时跳过节流保证相册 [n/N] 递进可见 |
| EWMA α | 0.3 | 速度平滑；实测 10→100 突变时第 8 次采样收敛到 ~74.7，不跳变 |

## 3. 调优指南

### 家用宽带 / 直连
- `threads = 8` 起步，配合 `file_concurrency = 2`；连接池会自动为文件所在 DC
  开多条 TCP 连接，突破单连接限速。
- 若仍慢，先用 `docker compose exec tgshelf /tgshelf -health` 排除磁盘/网络因素，
  再对照 `data/tgshelf.log` 里的 getFile 计数与 FLOOD_WAIT 记录。

### 经代理
- **代理软件自身吞吐优先于一切调参**：先测代理裸速，再谈 bot 调优。
- HTTP 代理的握手超时由独立逻辑处理；socks5 与 mtproxy 走另一条路径。

### 服务器 / 大批量
- `file_concurrency = 4` 以上时留意 FLOOD_WAIT（累计 0 次 @ 10.5GiB，但样本有限）。
- `takeout = true` 可将导出流量与普通会话的限速计数隔离，适合批量归档场景。
- `include`/`exclude`/`kinds`/`min_size`/`max_size` 过滤器可显著减少无效流量。

## 4. 观测手段

| 指标 | 方法 |
|---|---|
| 内存 | `/proc/<pid>/status` 的 VmRSS（已验证 27.8MB 峰值不随文件增长） |
| 下载实时速率 | 进度卡片 EWMA 平滑值（α=0.3） |
| 平均速率 | 终态消息「平均 X MB/s」 |
| FLOOD_WAIT | floodWait 中间件计数（日志可见等待秒数） |
| 健康状态 | `/tgshelf -health`（session 可读 + 磁盘剩余 ≥10MB） |
| 日志 | `data/tgshelf.log`（1MB × 3 份轮转，自动脱敏） |
