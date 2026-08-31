# zcode-quick-web-forward

> **Pure-CLI driver.** This tool does not re-implement login or tunnels — it invokes
> the official ZCode runtime's own subcommands (`login`, `app-server`) exactly as
> the desktop client does, so you get the real Z.AI authorize link and ZCode's
> own web-remote / mobile link.

One-shot helper that downloads the **latest ZCode** runtime (the bundled
`glm/zcode.cjs` app-server), starts it, opens the **browser login flow**,
confirms the login, and finally prints a **mobile / remote access link** —
so you can drive ZCode from your phone.

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
4. **web-remote** → ZCode ships its **own mobile tunnel** (this is what the
   desktop's "continue on your phone" uses), surfaced via the engine:
   - `remoteUrl: https://zcode.z.ai/remote/v4` (or bigmodel/zcode.chatglm.site)
   - `webRemoteCallbackUrl: https://zcode.z.ai/web-remote/callback`
   - `relayWsUrl: https://zcode.z.ai/ws`
   The tool prints the real web-remote link, which is the genuine **mobile
   access URL** — no custom piko needed.

> **Notes / requirements**
> - The runtime expects **Node ≥ 22.5** (uses `node:sqlite`, runs under the
>   desktop's bundled Electron node). Set `ZCODE_NODE` / `--node` accordingly.
> - The `.deb` has **no nodejs dependency** — ZCode bundles its own engine.
> - Completing login requires you to **click the authorize link in a browser**;
>   the client confirms on the callback.
> - The mobile link is ZCode's own web-remote (`https://zcode.z.ai/remote/v4…`);
>   it is established by the app-server, not by this tool.

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