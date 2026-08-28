# zcode-quick-web-forward

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
# full flow: download latest ZCode → spawn app-server → login → mobile link
zcode-quick-web-forward run

# just download/resolve the latest ZCode runtime
zcode-quick-web-forward download

# app-server + local web hub only (no tunnel)
zcode-quick-web-forward serve

# app-server + login link only
zcode-quick-web-forward login

zcode-quick-web-forward --help
```

### Flags

| flag | description |
|------|-------------|
| `--runtime-path PATH` | explicit glm runtime dir (env `ZCODE_RUNTIME_PATH`) |
| `--port PORT` | local web-hub port (default: ephemeral) |
| `--host HOST` | local hub bind host (default `127.0.0.1`) |
| `--tunnel MODE` | `none` \| `local` \| `piko` \| `ssh` \| `auto` (default `auto`) |
| `--tunnel-cmd "CMD"` | tunnel command override |
| `--node PATH` | node binary (env `ZCODE_NODE`) |

## What the binary does

1. **Download the latest ZCode** — resolves the newest desktop release from the
   ZCode update manifest / CDN for the running platform+arch, downloads and
   extracts the bundled `glm/zcode.cjs` app-server runtime into the user cache.
   Reuses an already-installed ZCode desktop app when present.
2. **Start the app-server** — spawns `node glm/zcode.cjs app-server` and talks
   to it over its newline-delimited JSON protocol.
3. **Login link + confirm** — prints the browser OAuth login URL, then confirms
   login on **Enter** or when an auth-success event is auto-detected.
4. **Local web hub** — serves a small status page (`/` and `/api/state`) on
   the local machine.
5. **Mobile / remote link** — forwards the local hub to the phone via a tunnel
   (`piko` / `ssh -R` / local) and prints the URL.

## gh.proxy

A tiny Git/proxy helper for China. Sourced usage:

```bash
source gh.proxy          # sets $GH_PROXY to a mirror
ghclone friddle/opencode # clone through the mirror
ghfetch <url> <dst>      # download through the mirror
```

Override the mirror with: `GH_PROXY=https://gh-proxy.com` (or `https://ghp.ci`,
`https://ghproxy.net`).

## License

MIT. This project is unofficial and not affiliated with or endorsed by Z.AI.
ZCode and its bundled runtime remain subject to their upstream terms.