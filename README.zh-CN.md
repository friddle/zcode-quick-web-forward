# zcode-quick-web-forward

[English](README.md) | 简体中文

> ⚠️ **BETA 测试版 — 使用需自担风险** ⚠️
>
> 本项目处于**活跃开发中的 beta 阶段**。它对话的 web-remote 协议是从 ZCode
> 官方客户端逆向出来的，随时可能随官方更新而失效。请预期各种粗糙边缘：
> 偶发的会话异常、守护进程需要重启恢复、手机端 UI 小毛病。它会在你的真实
> 机器上执行真实任务——请像对待任何 beta 自动化工具一样盯着它干活。
> **暂时不建议作为你操作 ZCode 的唯一途径。**

> **纯 CLI 驱动器。** 本工具不重新实现登录或隧道——它调用官方 ZCode 运行时
> （`glm/zcode.cjs` `app-server`），读取你真实的 ZCode 状态（任务索引、模型
> provider、设置），并在 ZCode 官方 web-remote 中继上生成手机配对链接——
> 手机看到的就是桌面端同样的工作区和任务。

一键式助手：下载**最新 ZCode** 运行时、完成登录（Z.AI OAuth 或 BigModel
API key）、暴露本地 ZCode 安装里**真实的**工作区/任务，并打印手机配对
URL。没有假数据——手机拿到的是 `~/.zcode` 里的真实内容（任务索引、provider
配置、设置）。

> 要不是为了 ZCode 编程套餐的优惠（usage×2 及各类活动），这个项目根本
> 不会存在。

单个 **Go** 静态二进制（Linux / macOS / Windows、多架构），外加
`wget | bash` 引导脚本（附带中国 / GFW 网络的 **gh.proxy** 镜像助手）。

```
ZCode 运行时 (glm/zcode.cjs) ── 登录 ──>  Z.AI / BigModel
        │
        ├── 读取 ~/.zcode：tasks-index.sqlite、provider 配置、设置
        └── web-remote 中继 (zcode.z.ai) ──>  手机配对 URL
```

## 快速开始（全球）

```bash
curl -fsSL https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/install.sh | bash
# 或
wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/install.sh | bash
```

中国 / GFW 用户（走代理镜像）：

```bash
wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/gh.proxy | bash
```

脚本会：
1. 探测操作系统/架构与网络区域（google/baidu 探测——仅中国网络走代理镜像
   下载 GitHub 资源），
2. 从 GitHub Releases 下载对应的预编译二进制（有 Go 环境时也可从源码构建），
3. 安装到 `~/.local/bin`，
4. 执行完整流程：区域 + 登录 + 工作区，然后打印手机配对 URL。

## 手动用法

```bash
# 交互式：区域（china/global）、登录方式（链接或 BigModel key），
# 然后启动引擎 + 中继并打印手机配对 URL。
# 缺失时 Node.js 与 ZCode 运行时自动下载。
zcode-quick-web-forward run

# 固定区域：决定登录方式与下载镜像
#   --region china  -> BigModel API key 登录 + 阿里云 node 镜像
#   --region global -> Z.AI OAuth 登录 + 官方 node 源
zcode-quick-web-forward run --region china
zcode-quick-web-forward run --region global

# 运行官方登录命令（Z.AI OAuth）：
zcode-quick-web-forward logincli

# 启动引擎 + 中继并打印手机配对 URL：
zcode-quick-web-forward remote

# 只下载/解析最新 ZCode 运行时
zcode-quick-web-forward download

zcode-quick-web-forward --help
```

**Node.js 无需自备**：运行时需要 `node` >= 22.5（用到了内置的
`node:sqlite`）。PATH 上没有或版本过旧时，工具会把托管版 Node.js 下载到
用户缓存——中国走阿里云镜像，其他地区走 nodejs.org。用 `--node` /
`ZCODE_NODE` 可强制指定自己的二进制。

### 工作区

暴露给手机的是你 ZCode 安装里**真实的**工作区，外加显式传入的。不给参数时
自动使用启动目录：

```bash
# 显式工作区（flag 可重复，或用路径分隔符的环境变量）：
zcode-quick-web-forward run --workspace /path/to/proj-a --workspace /path/to/proj-b
ZCODE_WORKSPACE=/a:/b zcode-quick-web-forward run
```

ZCode 任务索引（`tasks-index.sqlite`）里的本地工作区会自动合并进来。远程
SSH 工作区会被跳过（手机无法桥接过去）。

### 区域（国内 / global）

`--region china`（或 `--region global`）一次性决定所有网络相关行为；不带
flag 时**自动探测**——先探 google.com 再探 baidu.com（环境变量覆盖：
`ZCODE_REGION`）：

| | `china` | `global` |
|---|---|---|
| 登录 | BigModel API key（`open.bigmodel.cn`） | Z.AI OAuth（`chat.z.ai`） |
| Node.js 下载 | 阿里云 `mirrors.aliyun.com/nodejs-release` | `nodejs.org/dist` |

web-remote / 手机配对中继两个区域都默认 `https://zcode.z.ai`（环境变量
覆盖：`ZCODE_BASE_URL`）。

### 模型模式：ZCode 客户端（套餐）vs API Key

登录方式决定引擎如何接入 GLM——以及怎么计费：

| | 1) 登录链接 (ZCode 客户端模式) | 2) API Key (直连模式) |
|---|---|---|
| 登录 | Z.AI OAuth（手机号/短信，链接在任意浏览器打开） | BigModel API key（[open.bigmodel.cn/apikeys](https://open.bigmodel.cn/apikeys)） |
| 计费 | **编程套餐**：含 usage×2、各类优惠活动 | 标准 API 按量计费 |
| 依赖 | docker + chrome-driverless 镜像（网关验证码由真实浏览器产生，`install chrome` 一键装） | 无 |
| 网络 | 走 zcode-plan 网关 | 直连 `open.bigmodel.cn/api/anthropic` |

> **API Key 模式的重要限制**：智谱编程套餐的 **usage×2 / 限时加量 / 各种优惠**
> 只在套餐网关侧生效。直连 key 模式绕过了网关（也因此无需浏览器验证码），
> 所以这些优惠**全部不可用**，按 BigModel 标准 token 单价扣费。

第三方/自建 provider（SiliconFlow、DeepSeek、本地网关等）同样走 key 模式：

```bash
zcode-quick-web-forward provider add siliconflow \
    --base-url https://api.siliconflow.cn/v1 --api-key sk-xxx \
    --model deepseek-ai/DeepSeek-V3.1
zcode-quick-web-forward provider list   # 查看已配置的 provider
```

`provider add` 后重启 daemon，在手机 **管理模型** 里选择对应模型即可。

### 手机端能做什么（beta）

- **完整任务闭环**：手机提交任务、在桌面引擎上实时看流式输出、停止 / 编辑 /
  排队追问、审批权限请求——与桌面端相同的会话状态。
- **实时会话流**：官方 v4 快照 + 增量帧，合成自真实 host 状态
  （`zcode-session.readSession`、`conversationRowsRangeV4`），增量原地打补丁。
- **自愈传输**：daemon/host 重启后自动完成握手引导、卡队列检测与重提交、
  监听代际追踪（页面刷新后总能拿到全新全量快照）。

### 已知的 beta 限制

- web-remote v4 协议是**逆向出来的**；ZCode 客户端一次更新就可能破坏线上
  兼容性。
- daemon 的运行态在内存里（队列镜像、turn 跟踪、握手注册表）。重启后大部分
  自动恢复，但崩溃窗口内提交的消息可能需要重发。
- 手机上的模型选择器标签来自页面自己的 provider registry；通过
  `provider add` 添加的自定义 provider 的 id 可能对不上。
- 超大会话上的长任务比较重（全量日志读取）；轮询已限流但不免费。

### Flags

| flag | 说明 |
|------|------|
| `--runtime-path PATH` | 显式 glm 运行时目录（环境变量 `ZCODE_RUNTIME_PATH`） |
| `--node PATH` | node 二进制（环境变量 `ZCODE_NODE`）；可选——缺失/过旧时自动下载 |
| `--region REGION` | `china` / `global`（环境变量 `ZCODE_REGION`）；留空自动探测 |
| `--workspace PATH` | 暴露给手机的工作区（可重复；环境变量 `ZCODE_WORKSPACE`） |

## 它做了什么

1. **下载最新 ZCode** — 从 ZCode 更新清单 / CDN 解析最新桌面版，下载并解包
   附带的 `glm/zcode.cjs` 运行时到用户缓存。
2. **登录** — `--region china` 使用 **BigModel API key**（对
   `open.bigmodel.cn/api/anthropic/v1/models` 验证后写入
   `~/.zcode/v2/config.json` + `~/.zcode/cli/config.json`）；`--region global`
   运行官方 `login --no-browser`（Z.AI OAuth）。
3. **引擎** — 无头运行官方 ZCode 桌面 **host bundle**（与桌面应用相同的
   `zcode-host`），挂上 `zcode.cjs app-server` 引擎。
4. **web-remote** — `remote` 把本机注册为 ZCode 官方 web-remote 中继
   （`wss://zcode.z.ai/ws`）上的设备，打印**真实配对 URL**
   （`https://zcode.z.ai/remote/v4?sid=…&hash=…`），装了 `qrencode` 时还输出
   终端二维码。手机的频道服务（模型 provider、设置、任务）全部由**真实
   ZCode 状态**应答——任务索引（`tasks-index.sqlite`）、provider 配置、
   设置——会话管道则把手机页面桥接到官方 host，任务提交在真实引擎上执行。

> **说明 / 要求**
> - **Node.js 自动供给**：运行时需要 Node >= 22.5（用内置 `node:sqlite`）；
>   系统 node 缺失或过旧时自动下载托管版（中国走阿里云镜像，其他走
>   nodejs.org）。`ZCODE_NODE` / `--node` 可用自己的。
> - 配对中继默认 `https://zcode.z.ai`（可达）。有可公网解析的国内中继可用
>   `ZCODE_BASE_URL` 覆盖。
> - **Beta**：手机真实驱动桌面引擎——别把它指向一个「被 beta 工具碰一下也
>   无所谓」之外的工作区。

## Releases

每个 GitHub release 都附带静态二进制：`linux/amd64`、`linux/arm64`、
`darwin/amd64`、`darwin/arm64`、`windows/amd64`（由 `v*` 标签自动构建）。
**Release 属于 beta 质量**——升级常驻 daemon 前先读 release notes
（先 `systemctl stop zqf`；二进制原子替换后重启服务）。

## gh.proxy

中国 / GFW 助手，两件事：

1. **独立安装器** — `wget .../gh.proxy | bash` 的安装流程与 `install.sh`
   完全一致，只是中国网络下（google/baidu 探测）**所有** GitHub 下载走
   代理镜像：

   ```bash
   wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/gh.proxy | bash
   ```

2. **可 source 的 Git/代理包装器** — 任何 GitHub clone/下载复用镜像：

   ```bash
   source gh.proxy              # 仅中国网络下把 $GH_PROXY 设为镜像
   ghclone friddle/opencode     # 走镜像 git clone
   ghfetch <url> <dst>          # 走镜像下载
   ```

镜像可用 `GH_PROXY=https://gh-proxy.com` 覆盖（或 `https://ghp.ci`、
`https://ghproxy.net`）。Git 也可以全局配置镜像：

```bash
git config --global url."https://gh-proxy.com/https://github.com/".insteadOf "https://github.com/"
```

## 许可证

MIT。本项目为非官方 **beta** 项目，与 Z.AI 无关联亦未获其背书。ZCode 及其
捆绑运行时遵从其上游条款。
