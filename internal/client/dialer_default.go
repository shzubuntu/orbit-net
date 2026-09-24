//go:build !linux

package client

import "net"

// newDefaultDialer 非 Linux 平台普通拨号器。
func newDefaultDialer() dialer {
	return &net.Dialer{}
}
