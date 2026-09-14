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

### 修复方案（待指令后实施）

- **a) 重启恢复**：daemon 启动 / relay 重连后，对已知 session（索引最近任务 +
  `clientBySession`）逐个拉一次 rows，恢复 `turnRunning` 真实状态，消除重启盲区。
- **b) 覆盖所有 turn 启动路径**：queued send / sendQueuedNow / followup 续跑也走
  乐观标记 + refresher（现在只有 sendText 直发路径有）。
- **c) 可选**：`turnRunning` 持久化到 state 文件，重启后先按文件恢复再校正。

### 验证方式（修复后）

手机视口（390×844 + hover:none/pointer:coarse）：提交 30s 任务 → 主页卡片应为
「运行中」+ 转圈；结束后翻转「已完成」+ 蓝点；打开任务后蓝点消失。

### 状态

- [ ] 修复代码
- [ ] 部署
- 等待与其余任务一起做。

---

## 附：已知限制（非 bug，记录备查）

- **8899 备用镜像页**在公网 HTTP 下会 AUTH_FAILED（非 secure context，
  页面鉴权用的 `crypto.subtle` 不存在），只能 localhost/HTTPS 使用。服务已按
  用户要求关闭，恢复方法见 `scripts/web-remote-mirror/README.md`。
- relay 同一设备同一时间只允许一个终端页：多端互踢是官方设计。测试/调试连接
  会顶掉手机页面（本次已注意，调试前先知会）。
