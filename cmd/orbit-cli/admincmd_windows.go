//go:build windows

package main

import "syscall"

// CREATE_NO_WINDOW SysProcAttr.CreationFlags 位标志(常数在新版 syscall 包中未定义, 硬编码)。
const createNoWindow = 0x08000000

// setDetached Windows: 子进程不带控制台窗口(避免自拉起时弹黑框)。
func setDetached(attr *syscall.SysProcAttr) {
	attr.CreationFlags |= createNoWindow
}