//go:build !windows

package main

import "syscall"

// setDetached Linux/Unix: 新会话分离, 让守护进程独立于终端存活。
func setDetached(attr *syscall.SysProcAttr) {
	attr.Setsid = true
}