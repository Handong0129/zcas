#!/bin/bash
# 一键发版：同步版本号到各文件 → 提交 → 打 tag → 推送。
# tag 推送后 GitHub Actions（release.yml）自动构建三端产物并发布 Release（含自动更新日志）。
#
# 用法: scripts/release.sh 0.4.0
#
# 前置条件：工作区干净、当前分支已与 origin 同步（脚本会校验）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

# ---------- 参数校验 ----------
V=${1:-}
if [[ ! "$V" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "用法: scripts/release.sh <x.y.z>   如: scripts/release.sh 0.4.0" >&2
  exit 2
fi
TAG="v$V"

# ---------- 状态校验 ----------
if [[ -n "$(git status --porcelain)" ]]; then
  echo "✗ 工作区有未提交改动，请先提交或 stash" >&2
  exit 1
fi
git fetch origin --quiet
BRANCH=$(git branch --show-current)
if [[ "$(git rev-parse HEAD)" != "$(git rev-parse origin/$BRANCH)" ]]; then
  echo "✗ 本地 $BRANCH 与 origin/$BRANCH 不一致，请先 push/pull 对齐" >&2
  exit 1
fi
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null || git ls-remote --exit-code --tags origin "$TAG" >/dev/null 2>&1; then
  echo "✗ tag $TAG 已存在" >&2
  exit 1
fi

# ---------- 递增版本号（全局配置唯一来源）+ 派生 wails.json/workflow ----------
OLD=$(python3 scripts/config.py version)
echo "→ $OLD → $V"
python3 - "$V" <<'EOF'
import json, pathlib, sys
p = pathlib.Path("internal/buildinfo/project.json")
cfg = json.loads(p.read_text())
cfg["version"] = sys.argv[1]
p.write_text(json.dumps(cfg, ensure_ascii=False, indent=2) + "\n")
EOF
bash scripts/sync-config.sh

# ---------- 提交 + tag + 推送 ----------
git add internal/buildinfo/project.json app/wails.json .github/workflows/release.yml
git commit -m "release $TAG"
git tag "$TAG"
git push origin "$BRANCH" "$TAG"

echo "✅ $TAG 已推送，CI 构建中："
echo "   https://github.com/Handong0129/zcas/actions/workflows/release.yml"
echo "   完成后产物在：https://github.com/Handong0129/zcas/releases/tag/$TAG"
