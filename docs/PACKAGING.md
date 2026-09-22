# 打包与分发指南

## 一、一键全平台（本机即可）

```bash
make release
```

产出 `dist/` 下五个文件：

| 产物 | 平台 | 用户安装方式 |
|---|---|---|
| `zcas-0.2.0-macos-arm64.pkg` | macOS M 系列 | 双击安装器，自动装 App 到 `/Applications` + CLI 到 `/usr/local/bin`，零配置 |
| `zcas-0.2.0-macos-arm64.dmg` | macOS M 系列 | 拖拽安装（仅 GUI，不含 CLI） |
| `zcas-0.2.0-windows-amd64-installer.exe` | Windows x64 | 双击 NSIS 安装器，自动装 GUI（开始菜单/桌面快捷方式） |
| `zcas-0.2.0-windows-amd64-cli.zip` | Windows x64 | 解压即 CLI（`zcas.exe`），建议放到 PATH |
| `zcas-0.2.0-linux-arm64.tar.gz` | Linux ARM64 | 解压后 `sudo ./install.sh`，自动装 CLI + GUI + 桌面入口 |

单独构建：`make release-mac` / `make release-windows` / `make release-linux`
（Linux 出 x86_64：`ARCH=amd64 make release-linux`，走 Rosetta 模拟，较慢）。

### 各平台本机构建原理

- **macOS**：原生编译，pkgbuild 打包（payload 直接落位，无需脚本）。
- **Windows**：Wails 的 WebView2 绑定是纯 Go 实现，Mac 可直接交叉编译；
  安装器由 brew 装的 `makensis` 生成。
- **Linux**：GUI 依赖 WebKitGTK 原生库无法交叉编译，用 Docker
  （`scripts/build-linux-docker.sh`）在 Ubuntu 24.04 容器里编译，
  构建参数 `-tags webkit2_41`（Ubuntu 24.04 只有 4.1）。

### 收件人注意事项

- **macOS（无开发者账号，未公证）**：首次打开右键 → 打开；或
  `xattr -dr com.apple.quarantine /Applications/ZCode账号切换.app`。
- **Windows**：未签名的 exe 可能被 SmartScreen 提示，选"仍要运行"。
  用户机器需有 WebView2 运行时（Win11 自带，Win10 大多已装）。
- **Linux**：GUI 运行需要 `sudo apt install libwebkit2gtk-4.1-0`（install.sh 有提示）。

### Windows / Linux 上 OAuth 添加账号的说明

`zcode://` 协议自动接管目前只在 macOS 实现。Windows/Linux 上浏览器登录后
不会自动跳回工具，**复制地址栏的回调链接粘贴到工具里**即可完成（界面有输入框）。
后续可在 Windows 加注册表写入、Linux 加 x-scheme-handler 实现自动跳回。

## 二、GitHub Actions（可选）

`.github/workflows/release.yml` 已内置：`git tag v0.2.0 && git push origin v0.2.0`
后自动构建三平台并发布 Release。本机已能全平台出包，CI 主要用于自动化发布。

## 三、替换应用图标

```bash
scripts/make-icon.sh /path/to/新图标.png
make release
```

一条命令同时更新四处：macOS `icon.icns`、Windows `icon.ico`（PNG-in-ICO，
无第三方依赖）、Linux `icon.png`、窗口内图标 `appicon.png`。

## 四、版本号

`make release VERSION=0.3.0`（默认取 Makefile 里的 `VERSION`）。
