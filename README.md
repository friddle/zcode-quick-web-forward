# zcode-quick-web-forward

> **Pure-CLI driver.** This tool does not re-implement login or tunnels — it
> invokes the official ZCode runtime (`glm/zcode.cjs` `app-server`), reads your
> real ZCode state (task index, model providers, settings), and mints a phone
> pairing URL on ZCode's own web-remote relay — so the phone sees the same
> workspaces and tasks your desktop does.

One-shot helper that downloads the **latest ZCode** runtime, logs you in (Z.AI
OAuth or a BigModel API key), exposes the **real** workspaces/tasks from your
local ZCode install, and prints the phone pairing URL. No stubs — the phone
gets real data from `~/.zcode` (task index, provider config, settings).

A single **Go** static binary (built for Linux, macOS, Windows, multiple
arches) plus `wget | bash` bootstrap scripts (with a China / GFW **gh.proxy**
mirror helper).

```
ZCode runtime (glm/zcode.cjs) ── login ──>  Z.AI / BigModel
        │
        ├── reads ~/.zcode: tasks-index.sqlite, provider config, settings
        └── web-remote relay (zcode.z.ai) ──>  phone pairing URL
```

## Quick start (global)

```bash
curl -fsSL https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/install.sh | bash
# or
wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/install.sh | bash
```

China / GFW users (routed through a proxy mirror):

```bash
wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/gh.proxy | bash
```

The script:
1. detects OS/arch and the network region (google/baidu probe — GitHub
   downloads go through a proxy mirror only in China),
2. downloads the matching prebuilt binary from GitHub Releases (or builds from
   source when Go is available),
3. installs it to `~/.local/bin`,
4. runs the full flow: region + login + workspace, then the phone pairing URL.

## Manual usage

```bash
# interactive: region (china/global), login method (link or BigModel key),
# then engine + relay + phone pairing URL.
# Node.js and the ZCode runtime download automatically when missing.
zcode-quick-web-forward run

# pin the region: login method + download mirrors
#   --region china  -> BigModel API key login + Aliyun node mirror
#   --region global -> Z.AI OAuth login + official node sources
zcode-quick-web-forward run --region china
zcode-quick-web-forward run --region global

# run the official login command (Z.AI OAuth):
zcode-quick-web-forward logincli

# start engine + relay and print the phone pairing URL:
zcode-quick-web-forward remote

# just download/resolve the latest ZCode runtime
zcode-quick-web-forward download

zcode-quick-web-forward --help
```

**Node.js is handled for you**: when no suitable `node` (>= 22.5 — the runtime
uses the `node:sqlite` built-in) is on `PATH`, the tool downloads a managed
Node.js release into the user cache — through the Aliyun mirror in China,
nodejs.org elsewhere. Force your own binary with `--node` / `ZCODE_NODE`.

### Workspaces

The workspaces exposed to the phone are the **real** ones from your ZCode
install plus any you pass explicitly. When nothing is given, the startup
directory is used automatically:

```bash
# explicit workspaces (repeatable flag or env, path-separated):
zcode-quick-web-forward run --workspace /path/to/proj-a --workspace /path/to/proj-b
ZCODE_WORKSPACE=/a:/b zcode-quick-web-forward run
```

Local workspaces from the ZCode task index (`tasks-index.sqlite`) are merged
in automatically. Remote SSH workspaces are skipped (the phone can't bridge to
them).

### Region (国内 / global)

`--region china` (or `--region global`) selects everything network-related at
once; without the flag the region is **auto-detected** by probing
google.com, then baidu.com (env override: `ZCODE_REGION`):

| | `china` | `global` |
|---|---|---|
| login | BigModel API key (`open.bigmodel.cn`) | Z.AI OAuth (`chat.z.ai`) |
| Node.js download | Aliyun `mirrors.aliyun.com/nodejs-release` | `nodejs.org/dist` |

The web-remote / mobile pairing relay defaults to `https://zcode.z.ai` for
both regions (env override: `ZCODE_BASE_URL`).

### Model modes: ZCode client (套餐) vs API Key

The login method you pick decides how the engine reaches GLM — and what it
costs:

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

### Flags

| flag | description |
|------|-------------|
| `--runtime-path PATH` | explicit glm runtime dir (env `ZCODE_RUNTIME_PATH`) |
| `--node PATH` | node binary (env `ZCODE_NODE`); optional — auto-downloaded when missing/too old |
| `--region REGION` | `china` / `global` (env `ZCODE_REGION`); auto-detected when empty |
| `--workspace PATH` | workspace to expose to the phone (repeatable; env `ZCODE_WORKSPACE`) |

## What it does

1. **Download the latest ZCode** — resolves the newest desktop release from the
   ZCode update manifest / CDN, downloads and extracts the bundled
   `glm/zcode.cjs` runtime into the user cache.
2. **login** — `--region china` uses a **BigModel API key** (validated against
   `open.bigmodel.cn/api/anthropic/v1/models`, then written into
   `~/.zcode/v2/config.json` + `~/.zcode/cli/config.json`); `--region global`
   runs the official `login --no-browser` (Z.AI OAuth).
3. **engine** — runs `node glm/zcode.cjs app-server` (the ZCode engine).
4. **web-remote** — `remote` registers this machine as a device on ZCode's
   official web-remote relay (`wss://zcode.z.ai/ws`) and prints a **real
   pairing URL** (`https://zcode.z.ai/remote/v4?sid=…&hash=…`), plus a terminal
   QR code when `qrencode` is installed. The phone's channel services
   (model providers, settings, tasks) are answered from **real ZCode state** —
   the task index (`tasks-index.sqlite`), provider config and settings — so the
   phone shows actual workspaces and tasks.

> **Notes / requirements**
> - **Node.js is auto-provisioned**: the runtime needs Node >= 22.5 (it uses
>   the `node:sqlite` built-in); when the system node is missing or too old,
>   a managed release is downloaded (Aliyun mirror in China, nodejs.org
>   elsewhere). Set `ZCODE_NODE` / `--node` to use your own.
> - The pairing relay defaults to `https://zcode.z.ai` (reachable). Set
>   `ZCODE_BASE_URL` to a domestic relay if you have one that resolves
>   publicly.
> - Full engine-session control from the phone (running tasks on the desktop
>   engine) is not implemented yet — the phone gets a real workspace view.

## gh.proxy

A China / GFW helper that does two things:

1. **Standalone installer** — `wget .../gh.proxy | bash` installs
   `zcode-quick-web-forward` exactly like `install.sh`, but routes **every**
   GitHub download through a proxy mirror when on a China network
   (google/baidu probe):

   ```bash
   wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/gh.proxy | bash
   ```

2. **Sourced Git/proxy wrapper** — reuse the mirror for any GitHub clone/grab:

   ```bash
   source gh.proxy              # sets $GH_PROXY to a mirror only in China
   ghclone friddle/opencode     # git clone through the mirror
   ghfetch <url> <dst>          # download through the mirror
   ```

Override the mirror with `GH_PROXY=https://gh-proxy.com` (or `https://ghp.ci`,
`https://ghproxy.net`). Git itself can also be set globally to a mirror:

```bash
git config --global url."https://gh-proxy.com/https://github.com/".insteadOf "https://github.com/"
```

## License

MIT. This project is unofficial and not affiliated with or endorsed by Z.AI.
ZCode and its bundled runtime remain subject to their upstream terms.
