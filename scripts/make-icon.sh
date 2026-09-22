#!/bin/bash
# 一键替换全平台图标。用法: scripts/make-icon.sh /path/to/新图标.png
# 覆盖: macOS(icon.icns) / Windows(icon.ico, PNG-in-ICO 无第三方依赖) /
#       Linux(icon.png) / 窗口内图标(appicon.png)
set -euo pipefail
SRC=${1:?用法: scripts/make-icon.sh <png路径>}
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP=$(mktemp -d)

# --- macOS: build/darwin/icon.icns ---
mkdir -p "$ROOT/app/build/darwin" "$TMP/icon.iconset"
for s in 16 32 128 256 512; do
  sips -z "$s" "$s" "$SRC" --out "$TMP/icon.iconset/icon_${s}x${s}.png" >/dev/null
  sips -z "$((s*2))" "$((s*2))" "$SRC" --out "$TMP/icon.iconset/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$TMP/icon.iconset" -o "$ROOT/app/build/darwin/icon.icns"

# --- 通用 256 PNG ---
sips -z 256 256 "$SRC" --out "$TMP/icon-256.png" >/dev/null

# --- Windows: build/windows/icon.ico（ICO 容器内嵌 PNG，Vista+ 支持）---
mkdir -p "$ROOT/app/build/windows"
python3 - "$TMP/icon-256.png" "$ROOT/app/build/windows/icon.ico" <<'PY'
import struct, sys
png = open(sys.argv[1], 'rb').read()
hdr = struct.pack('<HHH', 0, 1, 1)
entry = struct.pack('<BBBBHHII', 0, 0, 0, 0, 1, 32, len(png), 6 + 16)
open(sys.argv[2], 'wb').write(hdr + entry + png)
PY

# --- Linux 桌面图标 + 窗口内图标 ---
mkdir -p "$ROOT/app/build/linux"
sips -z 512 512 "$SRC" --out "$ROOT/app/build/linux/icon.png" >/dev/null
cp "$TMP/icon-256.png" "$ROOT/app/build/appicon.png"

rm -rf "$TMP"
echo "✅ 图标已更新: macOS(icon.icns) / Windows(icon.ico) / Linux(icon.png) / 窗口(appicon.png)"
echo "   重新执行 make release 后生效"
