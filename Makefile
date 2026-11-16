# ZCS (zcas) — 发布打包
#
# 常用:
#   make release            # 全平台: macOS(pkg+dmg) + Windows(nsis) + Linux(docker)
#   make release-mac        # 只出 macOS
#   make release-windows    # 只出 Windows（amd64+arm64，本机交叉编译 + makensis）
#   make release-linux      # 只出 Linux（Docker，arm64；amd64 见下行）
#   ARCH=amd64 make release-linux   # Linux x86_64（走 Rosetta，较慢）
#
# 换图标:
#   scripts/make-icon.sh /path/to/新图标.png   （然后重新 make release）
# 全局配置唯一来源：internal/buildinfo/project.json（改配置只动它）
APP      := $(shell python3 scripts/config.py displayName)
VERSION  ?= $(shell python3 scripts/config.py version)
ARCH     ?= arm64
WIN_ARCHES ?= amd64 arm64
DIST     := dist

.PHONY: release release-mac release-windows release-linux cli app pkg dmg sync clean


release: release-mac release-windows release-linux

# sync 派生无法运行时读配置的文件（wails.json、workflow 默认值）
sync:
	bash scripts/sync-config.sh

# ---------- macOS ----------
release-mac: cli app pkg dmg

cli:
	mkdir -p $(DIST)/cli
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(DIST)/cli/zcas ./cmd/zcas

app: sync
	cd app && wails build -clean -platform darwin/arm64

pkg: cli app
	ARCH=arm64 bash scripts/build-pkg.sh $(VERSION)

dmg: app
	ARCH=arm64 bash scripts/build-dmg.sh $(VERSION)

# ---------- Windows（WebView2 绑定是纯 Go，Mac 可交叉编译；NSIS 安装器用 brew 的 makensis）----------
# amd64=Intel/AMD 主流，arm64=ARM 原生；两套都出，命名与 CI(release.yml) 对齐。
release-windows: sync
	@for arch in $(WIN_ARCHES); do \
		echo "==> Windows $$arch"; \
		( cd app && LC_ALL=en_US.UTF-8 LANG=en_US.UTF-8 wails build -clean -platform windows/$$arch -o zcas-gui -nsis ) || exit 1; \
		mkdir -p $(DIST)/windows-$$arch; \
		GOOS=windows GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(DIST)/windows-$$arch/zcas.exe ./cmd/zcas || exit 1; \
		cp app/build/bin/zcas-$$arch-installer.exe $(DIST)/zcas-$(VERSION)-windows-$$arch-installer.exe; \
		( cd $(DIST)/windows-$$arch && zip -q -j ../zcas-$(VERSION)-windows-$$arch-cli.zip zcas.exe ); \
	done
	@echo "✅ Windows 安装器 + CLI（$(WIN_ARCHES)）: $(DIST)/"

# ---------- Linux（Docker 容器内编译，需要 Docker Desktop 运行中）----------
release-linux:
	bash scripts/build-linux-docker.sh $(ARCH) $(VERSION)

clean:
	rm -rf $(DIST)
