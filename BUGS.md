# BUGS — 待修复记录

> 按用户要求：只分析、不动线上。修复与部署等待指令，与其余任务一起排期。

---

## BUG-001: 手机任务卡片「运行中」缺菊花 spinner

- **报告**: 2026-09-14。预期规格：**蓝点 = 已完成**；**未完成还在运行 = 菊花状 loading**。
- **现状**: 已完成的任务蓝点正常；运行中的任务卡片没有菊花。

### 页面渲染事实（bundle 静态分析，index-nOVzQNKW.js）

- 卡片右侧状态徽章：`displayStatus` 驱动，`m4('running')` 返回 `$h` + `animate-spin`
  —— **页面本身对 running 是会渲染转圈 Loader 的**，链路是
  `displayStatus:"running"` → 徽章 菊花 + 「运行中」。
- 左侧大图标只在「正在打开任务」（switchingTaskKey）时转菊花，平时是静态图钉；
  蓝点（`unreadAt`）渲染在左图标的右上角。
- 即：只要列表 payload 里运行中的任务带上 `displayStatus:"running"`，菊花就会出现。

### 根因分析（代码级）

daemon 侧 `officialTaskRuntime()` 负责给列表卡片覆盖 `displayStatus`/`unreadAt`，
它的数据源 `turnRunning`/`completedAt`/`viewedAt` 全部是**内存态**，有三个盲区：

1. **daemon 重启清空内存态**（最可能的现场）
   - 重启时正在跑的 turn：索引行要等 host 首次完成才写入 → 卡片整个不出现，
     自然没有菊花；重启后才完成的 turn 观察断档 → 连蓝点也没有。
   - 用户测试时间线里 daemon 因部署重启过多次，吻合。
2. **非手机 sendText 路径启动的 turn 不可见**
   - `turnRunning` 只在 sendText 乐观标记或 rowsRange 拉取（observeTurnState）时更新。
     排队中的 send、sendQueuedNow、followup 自动续跑等路径，在页面不打开该会话时
     没有任何 rows 拉取 → 运行状态永远不被观察 → 不显示菊花。
3. **页面左侧大图标不随 running 转**（页面自身设计，daemon 侧无法改变）
   - 如果用户预期是左侧图标转菊花，那属于官方页面视觉设计，只能记录为产品差异。

### 修复状态：**代码已写（official_rescue.go 等），见下方「修复实施」**

- **a) 重启恢复** ✅：bridge-open 时调用 `rescueRecentTurns(12)`，对索引里最近的
  任务逐个补一次 rows 观察，恢复 turnRunning（运行中的卡片恢复菊花徽章）。
- **b) 覆盖 sendQueuedNow** ✅：强制派发队列项时乐观标记 running + 启动 refresher。

---

## BUG-002: 「提交不成功」——turn 异常结束后消息被静默吞掉（已修代码，待部署观察）

- **报告**: 2026-09-14。用户把一段分析任务提交到「分析 bi overview retention 留存数据偏低原因」
  （sess_2e76d6aa），多次提交都没有反应。

### 日志取证时间线

- 13:07–13:10 会话正常跑了 turn，但**最后一个 assistant 消息以
  `AiSdkModelAdapterError` 结束**（模型 API 调用失败，13:10:41）。
- 13:11–13:14 用户多次重试提交（call 71/108/115/99/92），daemon 全部 early-ack
  并转发； ourselves 的队列镜像也记录了 "1 queued"。
- 但 **engine 的 db.sqlite 里该会话在 13:10:42 之后 0 条新消息** —— 用户文本
  从未入库。host 侧 `resyncConversationV4 FAIL fault.subscription.notOwned`、
  大量 `fault.subscribe.sessionNotFound`（91 次）。

### 根因

turn 异常结束后 **host 的投影把 turnHeader 卡在 `running`（endedAt=None）**，
host 认为会话仍占用 → 后续 sendText 全部进入 host 内部队列**等待一个永远不会结束
的 turn**，永不派发；而 daemon 已经 early-ack（页面清空输入框，无任何报错），
消息静默消失。

### 修复实施（official_rescue.go）

- **卡队列自愈**：refresher 每 5s 巡检 turnRunning 会话；若队列镜像里有条目
  `admittedAt` 超过 60s 仍未投递（且 120s 内没自救过）：
  1. 注入一条不带 `expectedForegroundExecutionId` 的 `stop` 强制释放 stale turn；
  2. 6s 后把卡住的 queued 文本作为**全新 sendText envelope**（新 commandId）重投；
  3. 清掉本地队列镜像并推送列表。
- **sendQueuedNow 乐观标记**（同 BUG-001-b）。
- **重启盲区恢复**（同 BUG-001-a）。

### 修复状态：**已修复并部署，线上验证通过**

- 验证：重投的分析任务完整跑完（23+ 条消息无报错），产出
  `docs/数据流动方案.md`（断档记录 / 数据流动 / 已放弃 topic / 排障速查）。

### BUG-002 修订记录（2026-09-14 14:16 误杀事件）

第一版自愈把「运行中 + 队列有条目」一律当卡死处理，注入 stop **误杀了一个健康
的 turn**（14:16 的 model_request_cancelled；用户在手机上正常追加的消息本就该
排队等 turn 结束，不是卡死）。已修正：

- **运行中的会话绝不碰**——排队等 turn 结束是 followupMode=queue 的正常行为；
- 只对「turn 已结束但队列条目仍 45s+ 未投递」的会话重投（此时无东西可杀，
  直接以全新 sendText 重跑准入）；
- host 对 sendText 的最终拒绝（30s 准入 pend 以错误收场）现在会大声记日志
  （此前静默），便于日后取证。

### BUG-003: 队列 chip 点「立即/移除/编辑」后卡在界面上（已修）

- **现象**: 点击后 chip 不消失，页面像卡住，手动刷新才恢复。
- **根因**: 快照去重键只看 rows——队列镜像变了但引擎 rows 还没变时，
  下一个快照被 "rows unchanged" 吞掉，页面永远拿不到 chip 已移除的新状态。
- **修复**: applyQueueOp 改镜像后丢掉该会话的去重键，并在 300ms 后主动拉一次
  快照（强制发射）；sendQueuedNow 额外乐观标记 running + 启动 refresher，
  让「立即」派发的消息顺利过渡到运行态。

### BUG-004: 删除/编辑无法撤回正在运行的任务（已修）

- **现象**: 对已派发执行的消息点删除/编辑，任务照跑不误。
- **根因**: 镜像里该条目在 userInput row 出现时已退役，删除/编辑落到引擎只会
  回 queue.itemMissing——没有任何人去停 turn。
- **修复**: 队列删除/编辑未命中镜像、deleteTask 目标任务、editUserQuery 目标
  会话，三者只要 turn 正在运行就注入 bare stop（撤回 = 先停）。

---

## 附：已知限制（非 bug，记录备查）

- **8899 备用镜像页**在公网 HTTP 下会 AUTH_FAILED（非 secure context，
  页面鉴权用的 `crypto.subtle` 不存在），只能 localhost/HTTPS 使用。服务已按
  用户要求关闭，恢复方法见 `scripts/web-remote-mirror/README.md`。
- relay 同一设备同一时间只允许一个终端页：多端互踢是官方设计。测试/调试连接
  会顶掉手机页面（本次已注意，调试前先知会）。

