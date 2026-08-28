#!/usr/bin/env bash
# zcode-quick-web-forward installer.
#
# One-liners:
#   curl -fsSL https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/install.sh | bash
#   wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/install.sh | bash
#
# China / GFW one-liner (uses gh.proxy mirror):
#   wget -qO- https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main/gh.proxy | bash
#
# It: 1) detects platform/arch, 2) grabs the matching prebuilt single binary
# from GitHub Releases (or builds from source when Go is available), 3) installs
# it to ~/.local/bin, 4) starts the full "run" flow: download the latest ZCode
# runtime, spawn the app-server, print the login link, confirm, and print the
# mobile/remote link.
set -euo pipefail

# ---- mirror selection (China / GFW) ----------------------------------------
# Override with:  GH_PROXY=https://gh-proxy.com   or   GH_PROXY=https://ghp.ci
GH_PROXY="${GH_PROXY:-}"
raw_base="https://raw.githubusercontent.com/friddle/zcode-quick-web-forward/main"
rel_base="https://github.com/friddle/zcode-quick-web-forward/releases/latest/download"

# mirror prepends the proxy prefix to a GitHub URL when GH_PROXY is set.
mirror() {
  local u="$1"
  if [ -n "$GH_PROXY" ]; then
    case "$u" in
      https://gh-proxy.com/*|https://ghp.ci/*|https://ghproxy.net/*)
        for p in https://gh-proxy.com/ https://ghp.ci/ https://ghproxy.net/; do
          u="${u#"$p"}";
        done;;
    esac
    printf '%s/%s' "$GH_PROXY" "$u"
  else
    printf '%s' "$u"
  fi
}

dl() { # dl <url> <dst>
  local url="$1" dst="$2"
  echo "zcode: downloading $(basename "$dst")"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 20 --retry 3 -o "$dst" "$(mirror "$url")"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --timeout=20 -t3 -O "$dst" "$(mirror "$url")"
  else
    echo "zcode: need curl or wget" >&2; exit 1
  fi
}

# ---- platform / arch detection ---------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux*) os="linux";;
  darwin*) os="darwin";;
  mingw*|cygwin*|msys*|windows*) os="windows";;
  *) echo "zcode: unsupported OS: $os" >&2; exit 1;;
esac
case "$arch" in
  x86_64|amd64) arch="amd64";;
  arm64|aarch64) arch="arm64";;
  armv7l|armhf) arch="arm";;
  *) echo "zcode: unsupported arch: $arch" >&2; exit 1;;
esac
ext=""; [ "$os" = "windows" ] && ext=".exe"
bin_name="zcode-quick-web-forward-${os}-${arch}${ext}"

# ---- install dir ------------------------------------------------------------
install_dir="${BIN_DIR:-${LOCALAPPDATA:-$HOME/.local/bin}}"
mkdir -p "$install_dir"
bin_path="$install_dir/zcode-quick-web-forward${ext}"

# ---- download prebuilt, else build from source ------------------------------
if dl "$rel_base/$bin_name" "$bin_path.tmp"; then
  :
else
  echo "zcode: no release asset, building from source…"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  if command -v git >/dev/null 2>&1; then
    git clone --depth 1 "$(mirror https://github.com/friddle/zcode-quick-web-forward.git)" "$tmp/src" \
      || { echo "zcode: failed to clone source" >&2; exit 1; }
  else
    dl "$raw_base/main.tar.gz" "$tmp/tar.gz"
    mkdir -p "$tmp/src"
    tar -xzf "$tmp/tar.gz" --strip-components=1 -C "$tmp/src" || {
      echo "zcode: failed to unpack source" >&2; exit 1; }
  fi
  if ! command -v go >/dev/null 2>&1; then
    echo "zcode: need 'go' on PATH to build from source" >&2; exit 1
  fi
  ( cd "$tmp/src" && CGO_ENABLED=0 go build -ldflags "-s -w" -o "$bin_path.tmp" ./cmd/zcode-quick-web-forward )
fi

chmod +x "$bin_path.tmp" 2>/dev/null || true
mv -f "$bin_path.tmp" "$bin_path"

echo "zcode: installed to $bin_path"
exec "$bin_path" run "$@"