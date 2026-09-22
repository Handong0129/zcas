#!/bin/bash
# 构建 macOS .dmg 拖拽安装镜像（仅 GUI；需要 CLI 的用户请用 .pkg）
# 用法: scripts/build-dmg.sh [版本号]
set -euo pipefail
VERSION=${1:-0.2.0}
APP="ZCode账号切换"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DMGDIR="$ROOT/dist/dmg-root"
ARCH=${ARCH:-arm64}

rm -rf "$DMGDIR"
mkdir -p "$DMGDIR"
cp -R "$ROOT/app/build/bin/zcas.app" "$DMGDIR/$APP.app"
ln -s /Applications "$DMGDIR/Applications"

hdiutil create -volname "ZCode 账号切换 $VERSION" \
  -srcfolder "$DMGDIR" -ov -format UDZO \
  "$ROOT/dist/zcas-$VERSION-macos-$ARCH.dmg"
rm -rf "$DMGDIR"
echo "✅ $ROOT/dist/zcas-$VERSION-macos-$ARCH.dmg"
