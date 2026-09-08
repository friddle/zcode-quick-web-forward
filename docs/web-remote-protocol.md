# Web Remote v4 协议笔记（网桥 ↔ 官方 web 客户端）

来源：官方桌面版 web-remote 会话抓包（2026-09-04，385 帧）+ `src-CT-oFXDw.js` /
`zcode-index.js` 里的 zod schema。金标准夹具见
`cmd/zcode-quick-web-forward/testdata/official-projection.json`，回归测试见
`main_test.go`。本文记录的是**网桥（桌面宿主角色）必须产出/应答**的部分。

## 传输层

- 页面 ↔ relay：WSS，web 客户端走 **binary frame**。载荷是 VSCode 风格的
  VSBuffer 序列化值（`internal/relay/channel.go` 的编解码）：
  `0=undefined 1=str 2=buffer 3=vsbuffer 4=array 5=object(JSON) 6=int(varint)`。
- 每条 WS 消息 = `serialize(head数组)` + `serialize(data值)` 直接拼接：
  - 客户端→宿主：`[100,id,channel,name] + [arg]`（Promise）、`[102,...]`（EventListen）
  - 宿主→客户端：`[200]`+`undefined`（Initialize）、`[201,id]`+`data`（成功）、
    `[202,id]`+`err`、`[204,id]`+`data`（EventFire 推送）
- 大消息分片（`dataBudget=48KiB`，≤64 片）+ crc32，见 relay/bridge.go。
  **分片上限超了会被静默丢弃**——恢复快照曾经因此整个到不了手机。

## 订阅主题（[204] EventFire 推送）

| topic | 信封 | payload |
|---|---|---|
| `conversation/<sessionId>` | wireVersion/kind/logicalFrameId… 包一层 | `{kind:snapshot\|deltas, ...}` |
| `sessions-index/<workspacePath>` | 同上包一层 | `{kind:snapshot\|deltas}` |
| `controller/tasks-index`、`controller/workspaces` | **不包**（扁平信封） | `{kind:snapshot\|deltas}` |

conversation 快照 keys（严格 zod，多字段会被 .strict() 拒收）：
`protocolVersion sessionId logEpoch seq revision control availability inputRouting
meta config modelTransition usage queue pendingInteractions pendingCommands
backgroundWorks subagents goal plan workspaceHookAdmission rows`。

### config（模型选择器/思考级别选择器的数据源）

```json
{"provider":"...","model":"GLM-5.3-Flash","thought":"high",
 "thoughtLevels":["low","high","max"],"followupMode":"queue","mode":"build"}
```
- **`thoughtLevels.length > 0` 才渲染思考级别选择器**（空数组 = 无此功能）。
  真实值取引擎 `session/read` 回包里的 `settings.thoughtLevel.available`。
- `usage.contextWindow` 取引擎 `runtime.contextUsage`（used/size）。

### control 三态（state.updated 扁平补丁，键是快照顶层键）

- running：`canStop:true, stopState:"stoppable", stopTargetKind:"assistant",
  activeWorks:[{kind:"primaryTurn",foregroundExecutionId,startedAt}]`，
  `inputRouting:{mode:"enqueue"}`，`availability.sendQueuedNow.allowed:true`
  → 驱动「工作中 N 秒 + 停止生成」。
- completed：`canStop:false, stopState:"idle", activeWorks:[]`，routing `startNow`，
  `sendQueuedNow:{allowed:false,reasonCode:"sendQueuedNowRequiresRunning"}`，
  `pauseGoal/resumeGoal` 恒为 `noGoalToPause/noGoalToResume`
  → 驱动「已处理」。
- usage/meta/config 补丁同为扁平键（如 `{meta:{title,titleSource},revision}`）。

### 行模型（rows.window / row.appended / row.upserted）

公共字段：`rowId turnId entityId productTurnId visibility createdAt createdAtSeq kind`。
- `turnHeader`：`origin:"userInput" executionKind:"agent" state startedAt endedAt
  activeMs historyRoundCount sourceCommandId`
- `userInput`：`text origin:"realUser" sourceCommandId rootSourceCommandId clientId`
- `assistantText` / `reasoning`：`assistantResponseId text state(complete|streaming)`
- `toolCall`：`toolCallId toolName status inputText input output{...} startedAt endedAt`
- `timelineMarker`：`lane:"lightBoundary" marker{type:"modelChange",fromProvider,
  fromModel,toProvider,toModel,toThought}` —— 模型切换时官方会推一行这个。

### 队列项（queue.items，严格 schema）

`sourceCommandId queueItemId clientId kind:"sendText" text attachments[] delivery
{requested,admitted} order{admissionSeq,queuePosition} steer{state} dispatch{state}
admittedAt`；快照里 `queue.autoDrain:true`。

## 会话命令（zcode-agent/sendConversationCommandV4 → ack）

```json
{"commandId":"...","status":"accepted","revisionAtDecision":0,
 "result":{"type":"inputAccepted","delivery":"startNow","inputId":"<commandId>"}}
```
- createSession → `result:{type:"createSession",sessionId}`
- 排队时 `delivery:"queue"`
- `deleteSession`：关引擎会话 + 任务标记删除 + 刷新列表/索引

## 引擎侧要点（zcode.cjs app-server）

- `session/read` 回包：`messages[]`（parts: text/reasoning/tool/timeline）、
  `session{title,model,workspace}`、`settings.thoughtLevel{available,current}`、
  `runtime.contextUsage{used,size,...}` —— 后三者喂给投影。
- `session/setModel` 严格 schema：只发 `{sessionId,model:{providerId,modelId},
  persistAsWorkspaceLastUsed}`；思考级别走独立的 `session/setThoughtLevel`。
- 引擎**不**流式吐正文：只有 `v4/telemetry` 的 `stream.chunk`（带 chunkLength 计数），
  转录在 turn.terminal 后由 `session/read` 一次性取回。

## 实时流式（v4/telemetry → conversation deltas，streaming.go）

- 引擎遥测事件种类：`stream.chunk`（channel=text|thought，**只有 chunkLength 无正文**）、
  `tool.lifecycle`（scheduled/started/progress/completed/failed + toolCallId/toolName/
  durationMs/errorCode）、`model.request.status`、`usage.delta`、`permission.lifecycle`、
  `turn.started`/`turn.terminal`、`subagent.lifecycle`、`compaction.terminal`；
  另有 `computer-use/operation-event`（tool 生命周期镜像）。
- 网桥映射：tool.lifecycle → 实时 toolCall 行（running→success/error 同 rowId upsert）；
  stream.chunk → 「思考中…/生成中… N 字」计数行（700ms 节流）。正文仍在 terminal 后整体同步。
- **seq 账本（客户端 v4-store 硬门控）**：deltas 帧要求 `frame.fromSeq == 客户端当前 seq`
  且 `toSeq` 更新；`toSeq<=seq` 静默丢弃，`fromSeq!=seq` 判为断档 → 自动重订阅恢复，
  **刚应用的行会被清掉**。快照把 seq 重置为 1；桥端 `convoFrameSeq` 账本维护连续递增。
  （历史 bug：fromSeq/toSeq 写死 1/2 → 每第二条 delta 触发恢复循环，实时行永远不显示。）
- **turnTailBoundary**：时间线只把「最后一个 `timelineMarker(lane=turnTailBoundary)` 之后」
  的行放进展开的实时尾部区，其余进折叠 history。每个 turn 的第一条实时行前要推一行
  不可见的边界行（`marker:{type:"checkpointRestored"}` 走渲染 default 分支 = 不显示）。
- toolCall 行 status 枚举（wire 值）：`inputStreaming/pendingApproval/running/success/
  error/cancelled`；reasoning/assistantText 行 `state: streaming|complete|interrupted`。
- delta op 全集：`row.appended / row.upserted / row.removed / row.delta{path:text|
  inputText|output.text|summaryText, append} / state.updated`。
- 手机侧行的 turnId 必须用 `turn-<phoneSid>`（与发送路径 turnHeader 一致）；
  引擎侧 `turn_xxx` 挂不到尾部区。
