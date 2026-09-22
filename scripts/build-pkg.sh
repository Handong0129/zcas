#!/bin/bash
# 构建 macOS .pkg 安装器：双击安装后自动完成——
#   ① App 装入 /Applications
#   ② CLI 装入 /usr/local/bin（系统默认 PATH，开箱即用）
# 用法: scripts/build-pkg.sh [版本号]
set -euo pipefail
VERSION=${1:-0.2.0}
APP="ZCode账号切换"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAGE="$ROOT/dist/pkg-root"
ARCH=${ARCH:-arm64}

rm -rf "$STAGE"
mkdir -p "$STAGE/Applications" "$STAGE/usr/local/bin"
cp -R "$ROOT/app/build/bin/zcas.app" "$STAGE/Applications/$APP.app"
cp "$ROOT/dist/cli/zcas" "$STAGE/usr/local/bin/zcas"
chmod 755 "$STAGE/usr/local/bin/zcas"

PKG="$ROOT/dist/zcas-$VERSION-macos-$ARCH.pkg"
pkgbuild --root "$STAGE" \
  --identifier "dev.zcas.installer" \
  --version "$VERSION" \
  --ownership recommended \
  "$PKG"
echo "✅ $PKG"
