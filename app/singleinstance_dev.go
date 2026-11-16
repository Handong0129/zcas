//go:build dev

package main

// singleInstanceID 单实例锁 ID（dev 构建）：与正式安装版区分，
// 否则 `wails dev` 会被同时运行的正式版「顶掉」（检测到已有实例后自行退出）。
const singleInstanceID = "zcas-9f3c1e50-single-instance-dev"
