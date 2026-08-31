# zcode-quick-web-forward

> **Pure-CLI driver.** This tool does not re-implement login or tunnels — it invokes
> the official ZCode runtime's own subcommands (`login`, `app-server`) exactly as
> the desktop client does, so you get the real Z.AI authorize link and ZCode's
> own web-remote / mobile link.

One-shot helper that downloads the **latest ZCode** runtime (the bundled
`glm/zcode.cjs` app-server) and starts it headless: run the **browser login
flow** (international Z.ai OAuth) or reuse the desktop client's existing
credentials/providers (e.g. the China BigModel API key), then keep the engine
serving. See [web-remote](#what-it-does-pure-cli-mimics-the-zcode-client) for
what the mobile link actually requires.

A single **Go** static binary (built for Linux, macOS, Windows, multiple
arches) plus `wget | bash` bootstrap scripts (with a China / GFW **gh.proxy**
mirror helper).

```
ZCode app-server (glm/zcode.cjs)  ──  login link ──>  browser OAuth
        │                                                    │
        └──────────── local web hub ── tunnel ──>  mobile/remote URL
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
1. detects OS/arch,
2. downloads the matching prebuilt binary from GitHub Releases (or builds from
   source when Go is available),
3. installs it to `~/.local/bin`,
4. runs the full flow.

## Manual usage

```bash
# full flow the installer starts: login -> app-server -> mobile/remote link
zcode-quick-web-forward run --node /path/to/node-24

# login with the real Z.AI OAuth (pure CLI, passes through the official client):
# prints an authorize URL -> open it in a browser -> authorize -> Enter to confirm
zcode-quick-web-forward logincli --node /path/to/node-24

# run the ZCode engine (the official app-server):
zcode-quick-web-forward app-server --node /path/to/node-24

# start app-server and print ZCode's own web-remote / mobile link:
zcode-quick-web-forward remote --node /path/to/node-24

# just download/resolve the latest ZCode runtime
zcode-quick-web-forward download

zcode-quick-web-forward --help
```

All commands need **Node ≥ 22.5** (the runtime uses `node:sqlite`; on this box
set `--node` to node 24 or `ZCODE_NODE`).

### China / BigModel (国内) users

The `logincli`/`run` OAuth flow targets the **international** Z.ai OAuth
(`chat.z.ai`) — the runtime's `login` subcommand has no BigModel variant. For
the China **BigModel** account you don't need OAuth at all:

1. The runtime shares the desktop client's config (`~/.zcode/v2/config.json`).
   Configure the BigModel API key once (the desktop client's "Bigmodel - API
   Key" provider, `baseURL https://open.bigmodel.cn/api/anthropic`, or an
   entry under `/provider/builtin:bigmodel`), or sign in via the desktop app.
2. Then start the engine directly — no login step:

   ```bash
   # 切换 web-remote / relay 到国内端点（默认国际站 zcode.z.ai）
   export ZCODE_BASE_URL=https://zcode.chatglm.site
   zcode-quick-web-forward remote
   ```

`ZCODE_BASE_URL` (or `ZCODE_ENDPOINT_ORIGIN` / `ZCODE_PRODUCTION_BASE_URL`)
overrides the runtime's service origin for web-remote/relay.

### Flags

| flag | description |
|------|-------------|
| `--runtime-path PATH` | explicit glm runtime dir (env `ZCODE_RUNTIME_PATH`) |
| `--node PATH` | node binary (env `ZCODE_NODE`), needs ≥22.5 |

## What it does (pure CLI, mimics the ZCode client)

This tool is a **thin passthrough** around the official ZCode runtime
(`glm/zcode.cjs`) — it does not re-implement login or tunnels. It invokes the
runtime's own subcommands exactly as the desktop client does:

1. **Download the latest ZCode** — resolves the newest desktop release from the
   ZCode update manifest / CDN for the running platform+arch, downloads and
   extracts the bundled `glm/zcode.cjs` runtime into the user cache. Reuses an
   already-installed ZCode desktop app when present.
2. **login** → runs `node glm/zcode.cjs login --no-browser`. Login **is** Z.AI
   OAuth; the runtime prints the real authorize URL
   (`https://chat.z.ai/api/oauth/authorize`, official `client_id`, with a
   **server-side** CLI callback `https://zcode.z.ai/api/v1/oauth/cli/callback/…`
   so it works on Linux too, not just a macOS `zcode://` scheme). You open the
   URL, authorize, and the CLI confirms on the callback / Enter.
3. **app-server** → runs `node glm/zcode.cjs app-server` (the ZCode engine, the
   "**ZCode Protocol stdio app server**").
4. **web-remote** → the mobile link (`https://<origin>/remote/v4?id=<session>`)
   is established by the **desktop client**, which authenticates to the cloud
   relay (`wss://<origin>/ws`, protocol version `2026-07-28`) and registers the
   session; the phone then joins through `https://<origin>/remote/v4`. The
   runtime itself never connects to the relay — a pure-CLI process cannot mint
   that link. `remote` therefore runs the engine and shows the link template;
   for actual phone access use the desktop client's "continue on your phone".

> **Notes / requirements**
> - The runtime expects **Node ≥ 22.5** (uses `node:sqlite`, runs under the
>   desktop's bundled Electron node). Set `ZCODE_NODE` / `--node` accordingly.
> - The `.deb` has **no nodejs dependency** — ZCode bundles its own engine.
> - Completing login requires you to **click the authorize link in a browser**;
>   the client confirms on the callback.
> - The mobile link is ZCode's own web-remote (`https://<origin>/remote/v4…`);
>   it is minted by the desktop client via the cloud relay, not by the
>   app-server and not by this tool.

## gh.proxy

A China / GFW helper that does two things:

1. **Standalone installer** — `wget .../gh.proxy | bash` installs
   `zcode-quick-web-forward` exactly like `install.sh`, but routes **every**
   GitHub download through a proxy mirror:

   ```bash
   wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/gh.proxy | bash
   ```

   (This is the file that is fetched by the `wget github.com/friddle/xxx.sh |
   bash` bootstrap in China.)

2. **Sourced Git/proxy wrapper** — reuse the mirror for any GitHub clone/grab:

   ```bash
   source gh.proxy              # sets $GH_PROXY to a mirror
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