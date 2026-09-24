// Package tun 虚拟网卡抽象: Read/Write 处理 IP 包,Configure 配置地址/MTU 并拉起。
// Linux 走 /dev/net/tun;其他平台暂返回 ErrUnsupported(M2 起补)。
package tun

import (
	"errors"
	"net"
)

// ErrUnsupported 当前平台未实现 TUN。
var ErrUnsupported = errors.New("tun device not supported on this platform yet")

// Device 虚拟网卡。
type Device interface {
	// Read 从 TUN 读一个 IP 包,返回长度。p 应足够大(MTU)。
	Read(p []byte) (n int, err error)
	// Write 把 IP 包写入 TUN(发到本机协议栈)。
	Write(p []byte) (n int, err error)
	Close() error
	// Name 设备名。
	Name() string
	// Configure 配置 IP/掩码/MTU 并拉起设备。
	Configure(ip net.IP, mask net.IPMask, mtu int) error
}
