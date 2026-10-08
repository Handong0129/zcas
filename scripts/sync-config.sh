#!/bin/bash
# 从全局配置 internal/buildinfo/project.json 派生"无法运行时读配置"的文件：
#   - app/wails.json（wails CLI 只认这个文件，字段在构建时读取）
#   - .github/workflows/release.yml（workflow_dispatch 的版本号默认值，YAML 无法动态取值）
# Go 代码经 buildinfo 内嵌、脚本经 config.py 读取，都不需要派生。
# 用法: scripts/sync-config.sh（改了 project.json 后运行；make app/release 会自动调用）
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

python3 - "$ROOT" <<'EOF'
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
cfg = json.loads((root / "internal/buildinfo/project.json").read_text())

# wails.json：只覆盖配置驱动的字段，其余（frontend 命令等）原样保留
wails_path = root / "app/wails.json"
w = json.loads(wails_path.read_text())
w["name"] = cfg["name"]
w["outputfilename"] = cfg["name"]
w["info"] = {
    "productName": cfg["displayName"],
    "productVersion": cfg["version"],
    "copyright": cfg["copyright"],
}
w["author"] = cfg["author"]
wails_path.write_text(json.dumps(w, ensure_ascii=False, indent=2) + "\n")

# release.yml：workflow_dispatch 的版本号默认值
wf_path = root / ".github/workflows/release.yml"
s = wf_path.read_text()
s = re.sub(r"^        default: '[^']*'$", f"        default: '{cfg['version']}'", s, flags=re.M)
wf_path.write_text(s)

print(f"✓ 已同步 wails.json / release.yml（{cfg['displayName']} v{cfg['version']}）")
EOF
