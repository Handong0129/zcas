#!/bin/bash
# 在 Docker 容器里构建 Linux 版（Wails Linux 依赖 WebKitGTK 原生库，无法直接交叉编译）。
# M 系列 Mac 跑 linux/arm64 容器是原生的；linux/amd64 走 Rosetta 模拟（较慢）。
# 用法: scripts/build-linux-docker.sh [arm64|amd64] [版本号]
set -euo pipefail
ARCH=${1:-arm64}
VERSION=${2:-0.2.0}
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
IMAGE="zcas-linux-builder:$ARCH"
OUT="$ROOT/dist/linux-$ARCH"

# 1. 构建构建环境镜像（首次较慢，之后有缓存）
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "→ 首次运行，构建 Linux 构建环境镜像（约几分钟）…"
  docker build --platform "linux/$ARCH" -t "$IMAGE" - <<'DOCKERFILE'
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates build-essential pkg-config git \
      libgtk-3-dev libwebkit2gtk-4.1-dev nodejs npm \
    && rm -rf /var/lib/apt/lists/*
ARG TARGETARCH
RUN GO_VER=$(curl -fsSL https://go.dev/VERSION?m=text | head -n1) \
    && curl -fsSL "https://go.dev/dl/${GO_VER}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:/root/go/bin:${PATH}"
# 国内网络环境：Go 模块走 goproxy.cn，npm 走 npmmirror
ENV GOPROXY="https://goproxy.cn,direct"
RUN npm config set registry https://registry.npmmirror.com \
    && go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0
DOCKERFILE
fi

# 2. 容器内构建 GUI + CLI（先复制到容器内文件系统再编译，避免 linux 版
#    node_modules 污染宿主机；挂缓存卷避免每次重新下载依赖）
rm -rf "$OUT" && mkdir -p "$OUT"
docker run --rm --platform "linux/$ARCH" \
  -v "$ROOT":/src -w / \
  -v zcas-gocache-$ARCH:/root/.cache/go-build \
  -v zcas-gomod-$ARCH:/root/go/pkg/mod \
  -v zcas-npm-$ARCH:/root/.npm \
  "$IMAGE" bash -c "
    set -e
    mkdir -p /build
    tar cf - --exclude=node_modules --exclude=dist -C /src . | tar xf - -C /build
    cd /build/app/frontend && npm ci --no-audit --no-fund
    cd .. && wails build -clean -platform linux/$ARCH -o zcas-gui -tags webkit2_41
    cd .. && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /src/dist/linux-$ARCH/zcas ./cmd/zcas
    cp app/build/bin/zcas-gui /src/dist/linux-$ARCH/
  "

# 3. 组装安装包（CLI=zcas，GUI=zcas-gui + 桌面入口 + 图标 + 安装脚本）
cp "$ROOT/app/build/linux/icon.png" "$OUT/icon.png"
cat > "$OUT/zcas.desktop" <<'DESKTOP'
[Desktop Entry]
Name=ZCode 账号切换
Comment=ZCode 多账号切换工具
Exec=zcas-gui
Icon=zcas
Type=Application
Categories=Utility;Development;
DESKTOP
cat > "$OUT/install.sh" <<'INSTALL'
#!/bin/bash
# zcas Linux 安装器：CLI 装到 /usr/local/bin/zcas，GUI 装桌面入口
set -e
PREFIX="${PREFIX:-/usr/local}"
echo "→ 安装到 $PREFIX（需要 sudo）"
sudo install -m755 zcas "$PREFIX/bin/zcas"
sudo install -m755 zcas-gui "$PREFIX/bin/zcas-gui"
sudo install -Dm644 zcas.desktop /usr/share/applications/zcas.desktop
sudo install -Dm644 icon.png /usr/share/icons/hicolor/512x512/apps/zcas.png
echo "✓ 完成：应用菜单搜「ZCode 账号切换」，终端用 zcas 命令"
echo "  如 GUI 无法启动，请先安装运行时: sudo apt install libwebkit2gtk-4.1-0"
INSTALL
chmod +x "$OUT/install.sh"

tar czf "$ROOT/dist/zcas-$VERSION-linux-$ARCH.tar.gz" -C "$OUT" .
echo "✅ $ROOT/dist/zcas-$VERSION-linux-$ARCH.tar.gz"
