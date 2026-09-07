#!/bin/sh
# Build the official-host deploy directory from the desktop app bundle.
# Usage: ./build-from-desktop.sh [=/Applications/ZCode.app/Contents/Resources/app.asar]
ASAR="${1:-/Applications/ZCode.app/Contents/Resources/app.asar}"
set -e
cd "$(dirname "$0")"
rm -rf host-deploy && mkdir -p host-deploy/host
npx --yes @electron/asar extract "$ASAR" /tmp/zcode-full
cp /tmp/zcode-full/out/host/*.js host-deploy/host/
for pkg in node-forge undici ws yaml yazl; do cp -R "/tmp/zcode-full/node_modules/$pkg" host-deploy/node_modules/ 2>/dev/null || true; done
mkdir -p host-deploy/node_modules/@larksuiteoapi
cp -R /tmp/zcode-full/node_modules/@larksuiteoapi/node-sdk host-deploy/node_modules/@larksuiteoapi/ 2>/dev/null || true
cd host-deploy
cat > package.json <<'JSON'
{"name":"zcode-official-host","private":true,"type":"module","dependencies":{
 "@larksuiteoapi/node-sdk":"^1","axios":"^1","node-forge":"^1","undici":"^6",
 "ws":"^8","yaml":"^2","yazl":"^2","math-intrinsics":"^1"}}
JSON
mkdir -p node_modules
npm install --omit=dev --no-audit --no-fund
cp ../shim.mjs .
echo "host-deploy ready — rsync to ~/.zcode/host-official on the remote"
