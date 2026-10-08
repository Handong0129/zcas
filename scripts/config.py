#!/usr/bin/env python3
# 读取工程全局配置 internal/buildinfo/project.json 的单个字段，供 Makefile/shell 脚本使用。
# 用法: scripts/config.py <key>     如: version、displayName、author.name
import json
import pathlib
import sys

cfg = json.loads((pathlib.Path(__file__).parent.parent / "internal/buildinfo/project.json").read_text())
node = cfg
try:
    for part in sys.argv[1].split("."):
        node = node[part]
except (KeyError, IndexError):
    sys.exit(f"用法: config.py <key>（可选: {', '.join(cfg)}；嵌套用点号如 author.name）")
print(node)
