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
