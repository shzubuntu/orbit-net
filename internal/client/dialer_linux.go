//go:build linux

package client

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// orbMark 出口流量 fwmark,与策略路由表配合防回环(部署时配 ip rule table)。
const orbMark = 0x517

// newDefaultDialer 返回带 SO_MARK 的拨号器(出口拨号包标记,避免被自己隧道吞掉)。
func newDefaultDialer() dialer {
	return &net.Dialer{
		Control: func(network, address string, c syscall.RawConn) error {
			var serr error
			if err := c.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, orbMark)
			}); err != nil {
				return err
			}
			return serr
		},
	}
}

var _ dialer = (*net.Dialer)(nil)
