# 架构债清单 — 虚假/取巧实现盘点与真实化路线

> 背景：部分功能最初用「伪造状态 + 快速补丁」实现并因此能工作（work），但
> 状态与真实源（engine / host 投影 / task-index）发散后，后续按真实语义修
> bug 时引发连锁异常（totalCount 修复 → 全量补拉循环 → 页面重载风暴；
> 自愈误判 → stop 误杀健康 turn）。本文件全局盘点这些实现，给出真实架构
> 目标与分阶段计划。**原则：daemon 是管道与恢复层，不是第二个投影引擎；
> 一切状态以 engine / host 为准，本进程内存态只允许做「缓存 + 可校正的
> 乐观值」，且必须有引擎真源闸门。**

## 图例

- 风险：🟥 状态发散会造成用户可见故障 🟧 影响单点体验 🟩 仅日志/外观
- 状态：✅ 已按真实架构实现 / 🔶 真源闸门已加（仍有合成） / ❌ 仍是伪实现

## A. 状态伪造（最危险）

| # | 实现 | 位置 | 伪造了什么 | 真实架构 | 风险 | 状态 |
|---|---|---|---|---|---|---|
| A1 | `turnRunning` map | official_snapshot.go `recordTurnRunning` | 用 turnHeader 行 + 乐观标记推断 turn 是否在跑 | engine 的 `session.status`（readSession 每 ~10s 轮询，已 stash）| 🟥 双向发散：误杀健康 turn（14:16 事故）/ 漏救 | 🔶 自愈已改用引擎真源闸门；`turnRunning` 仍驱动菊花徽章与 refresher |
| A2 | `queuedSends` 队列镜像 | official_snapshot.go `takeQueuedItems`/`applyQueueOp` | 自己维护「待发送队列」，与 engine 的真实队列（`queue_<commandId>`）平行 | host 投影 snapshot 的 `queue` 字段 / engine 队列查询 | 🟥 发散导致 chip 卡住、误判卡死、stop 误杀 | ❌ 差异窗口已缩小（镜像退役 + 去重键丢弃），真源化待做 |
| A3 | `completedAt`/`viewedAt`（结束蓝点） | official_types.go | 内存推断蓝点；task-index 的 `unread_at` headless 下无人写 | host 应在 turn 结束且无查看者时写 `unread_at` | 🟧 重启丢蓝点 | ❌（已记录已知限制）|
| A4 | `optimisticRun`/`optimisticRow`（乐观发送行） | official_track.go ~250 | 发送后伪造 userInput 行/running 态，骗页面即时反馈 | host 立即回显（桌面端正常；headless sendText pend ~30s 是根因）| 🟧 45s 窗口内与真实 rows 发散（有自校正）| 🔶 |
| A5 | `rescueRecentTurns`（bridge-open 重扫 12 个会话） | official_rescue.go | 主动拉 rows 恢复 turnRunning | host 的 sessions-index 应携带实时状态 | 🟩 只读观察，60s 限频 | 🔶 |

## B. 快照伪造（合成投影）

| # | 实现 | 位置 | 伪造了什么 | 真实架构 | 风险 | 状态 |
|---|---|---|---|---|---|---|
| B1 | 合成 conversation snapshot | official_snapshot.go `buildProjectionSnapshot` | 整个 v4 会话快照由 daemon 拼装（而非 host 官方投影帧）| host 官方投影（桌面 app 消费的同一路径）| 🟥 本次多次事故的总根源 | ❌ 结构性问题，见「北极星」 |
| B2 | 窗口 24 行 + 48KiB 字节预算 | official_snapshot.go | 历史被裁剪，只发最新尾巴 | host 全量窗口 + 页面分页 | 🟧 大会话历史不全（分页已接通）| 🔶 |
| B3 | `totalCount`=窗口长度、`firstRowId` 游标 | official_snapshot.go | 总数语义变弱（真实总数会导致页面全量补拉循环，600KB×N 拖死手机）| 真实 totalCount | 🟧 问题目录/计数不精确 | 🔶 有意为之，已记录 |
| B4 | 硬编码字段：`backgroundWorks:[]`、`subagents` 空、`plan:nil`、`slashCommands`… | official_snapshot.go ~320 | 相关 UI 的数据源是常量 | host 数据 | 🟩 功能缺失/不准 | ❌ |
| B5 | `pendingInteractions` 合成（权限卡 options） | official_snapshot.go ~195 | 选项文案/结构手工造 | engine 的 interaction 原文 | 🟧 选项与真实交互不一致 | ❌ |
| B6 | `usage`/`config`/`mode` 从 readSession 重构 | official_snapshot.go `parseSessionFacts` | —（读的就是 host/engine 数据，只是拼装）| — | 🟩 | ✅（这是「真实化」的正确姿势范本）|

## C. 协议时序补丁

| # | 实现 | 位置 | 补了什么 | 真实架构 | 风险 | 状态 |
|---|---|---|---|---|---|---|
| C1 | sendText early-ack + 队列镜像重投 | official_track.go ~250 / official_rescue.go | host 对 sendText pend ~30s 才答复，页面全局禁发送键 | host 快速应答 | 🟥 应答后消息可能被 host 丢弃（提交不成功事故）→ 已加 REJECTED 大声日志 + 引擎真源闸门自愈重投 | 🔶 |
| C2 | queue-op early-ack + `suppressAck` 吞引擎 ack | official_track.go ~130 / official_runtime.go ~170 | 首次队列操作引擎回 stale，页面会放弃流程 | 引擎 revision 应对外提供服务端当前值 | 🟧 引擎真实失败被掩盖（已改为：失败时撤回 stop）| 🔶 |
| C3 | `seenCommand` 去重（手机 transport 重投递同一 commandId） | official_forward.go ~285 | 传输层重发 | transport 不重发 | 🟩 | 🔶 必要垫片 |
| C4 | 撤回/删除/编辑运行中任务 → 注入 bare stop | official_track.go（withdraw）/ official_rescue.go | host 内部本应「先停再删/改」 | host 自身完成 stop+删除组合动作 | 🟥 曾误杀（14:16）→ 已加引擎真源闸门（running 不碰）| 🔶 |
| C5 | `rescueStuckQueues` 卡队列自愈（idle 时重投） | official_rescue.go | host 队列卡死后无人派发 | host 派发不卡 | 🟧 重投可能重复 → 已用引擎 status 闸门 + 120s 限频 | 🔶 |

## D. 任务列表伪造

| # | 实现 | 位置 | 伪造了什么 | 真实架构 | 风险 | 状态 |
|---|---|---|---|---|---|---|
| D1 | `taskListPayload` 覆盖 displayStatus/unreadAt | webremote.go | 用引擎推断覆盖索引状态 | host 维护 task_status/unread_at | 🟧 状态滞后已被引擎覆盖缓解 | 🔶 |
| D2 | `setTurnRunning`/`setSession`/`runtimeTask` 死代码 | sessions.go | — | 应删除或接通 | 🟩 误导维护者 | ❌ 待清理 |
| D3 | 运行中任务不在索引时的合成卡片 | official_types.go `officialSyntheticTasks` | 索引行 host 完成时才写，运行中无卡片 | host 应实时写索引 | 🟧 | 🔶（真源化前必要）|

## 北极星（真实架构目标）

1. **投影真源化**：v4 会话投影（snapshot/queue/control/interactions）应来自
   host 官方投影帧——即桌面 app 消费的同一条流。daemon 退役 `queuedSends`
   镜像与大部分快照合成，只保留：传输分片、断线恢复、真源闸门。
   前置验证：headless host 能否开启完整 v4 投影输出（`turnNavigator`/桌面
   使用的入口）。若可行，B 区整体退役。
2. **单一状态源**：`turnRunning`→`session.status`（readSession），
   `queuedSends`→host `queue`，蓝点→host 写 `unread_at`（需确认 headless
   行为或向官方提需求）。内存态降级为缓存。
3. **应答语义**：推动 host 快速应答 sendText（或 daemon 侧把「已受理」与
   「已派发」分离成两个可信事件），early-ack 退役。
4. **清理**：删除 D2 死代码；`suppressAck` 演进为「保存应答而非吞掉」。

## 阶段计划

- **阶段 0（已完成）**：自愈/撤回全部加引擎真源闸门（A1/C4/C5）。
- **阶段 1**：host `queue`/`control` 字段落库为唯一状态源，删除 `queuedSends`
  镜像的独立推导（A2）。
- **阶段 2**：headless 下 host 投影输出可行性验证（B1 北极星前置）。
- **阶段 3**：退役合成快照；清理死代码（D2）；蓝点真源化（A3）。
