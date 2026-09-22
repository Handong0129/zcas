# ZCode 账号切换 (zcas) — 发布打包
#
# 常用:
#   make release            # 全平台: macOS(pkg+dmg) + Windows(nsis) + Linux(docker)
#   make release-mac        # 只出 macOS
#   make release-windows    # 只出 Windows（本机交叉编译 + makensis）
#   make release-linux      # 只出 Linux（Docker，arm64；amd64 见下行）
#   ARCH=amd64 make release-linux   # Linux x86_64（走 Rosetta，较慢）
#
# 换图标:
#   scripts/make-icon.sh /path/to/新图标.png   （然后重新 make release）

APP      := ZCode账号切换
VERSION  ?= 0.2.0
ARCH     ?= arm64
DIST     := dist

.PHONY: release release-mac release-windows release-linux cli app pkg dmg clean

release: release-mac release-windows release-linux

# ---------- macOS ----------
release-mac: cli app pkg dmg

cli:
	mkdir -p $(DIST)/cli
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(DIST)/cli/zcas ./cmd/zcas

app:
	cd app && wails build -clean -platform darwin/arm64

pkg: cli app
	ARCH=arm64 bash scripts/build-pkg.sh $(VERSION)

dmg: app
	ARCH=arm64 bash scripts/build-dmg.sh $(VERSION)

# ---------- Windows（WebView2 绑定是纯 Go，Mac 可交叉编译；NSIS 安装器用 brew 的 makensis）----------
release-windows:
	cd app && wails build -clean -platform windows/amd64 -o zcas-gui -nsis
	mkdir -p $(DIST)/windows-amd64
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(DIST)/windows-amd64/zcas.exe ./cmd/zcas
	cp app/build/bin/zcas-amd64-installer.exe $(DIST)/zcas-$(VERSION)-windows-amd64-installer.exe
	cd $(DIST)/windows-amd64 && zip -q -j ../zcas-$(VERSION)-windows-amd64-cli.zip zcas.exe
	@echo "✅ Windows 安装器 + CLI: $(DIST)/"

# ---------- Linux（Docker 容器内编译，需要 Docker Desktop 运行中）----------
release-linux:
	bash scripts/build-linux-docker.sh $(ARCH) $(VERSION)

clean:
	rm -rf $(DIST)
