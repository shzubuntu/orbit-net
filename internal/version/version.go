// Package version 全局构建版本号。
// 正式构建由构建脚本通过 -ldflags "-X orbit/internal/version.Version=<git describe>"
// 注入, 开发环境为 "dev"。
package version

import "strings"

// Version 构建版本号。
var Version = "dev"

// NeedsUpdate 仅做相等性比较: local 为 dev / 任一侧为空时静默返回 false。
func NeedsUpdate(local, remote string) bool {
	local = strings.TrimSpace(local)
	remote = strings.TrimSpace(remote)
	return local != "" && local != "dev" && remote != "" && local != remote
}
