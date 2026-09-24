//go:build linux

package tun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tunABI 对应内核 struct ifreq。
type tunABI struct {
	Name  [16]byte
	Flags uint16
	_     [22]byte
}

const (
	iffTun  = 0x0001
	iffNoPi = 0x1000 // 不带头,直接读 IP 包
)

type linuxTun struct {
	fd   *os.File
	name string
}

// Create 打开 /dev/net/tun 并创建 TUN(需 CAP_NET_ADMIN)。
func Create(name string) (Device, error) {
	fd, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var req tunABI
	req.Flags = iffTun | iffNoPi
	if name != "" {
		if len(name) >= len(req.Name) {
			_ = fd.Close()
			return nil, fmt.Errorf("tun name too long: %s", name)
		}
		copy(req.Name[:], name)
	}
	const TUNSETIFF = 0x400454ca
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd.Fd(),
		TUNSETIFF, uintptr(unsafe.Pointer(&req))); errno != 0 {
		_ = fd.Close()
		return nil, fmt.Errorf("TUNSETIFF: %w", errno)
	}
	return &linuxTun{fd: fd, name: trimZero(req.Name[:])}, nil
}

func trimZero(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func (t *linuxTun) Read(p []byte) (int, error)  { return t.fd.Read(p) }
func (t *linuxTun) Write(p []byte) (int, error) { return t.fd.Write(p) }
func (t *linuxTun) Close() error                { return t.fd.Close() }
func (t *linuxTun) Name() string                { return t.name }

// Configure 用 ip 命令配置地址/MTU 并拉起。
func (t *linuxTun) Configure(ip net.IP, mask net.IPMask, mtu int) error {
	ones, _ := mask.Size()
	addr := ip.String() + "/" + strconv.Itoa(ones)
	if err := runIP("addr", "add", addr, "dev", t.name); err != nil {
		return fmt.Errorf("ip addr add: %w", err)
	}
	if err := runIP("link", "set", "dev", t.name, "mtu", fmt.Sprintf("%d", mtu)); err != nil {
		return fmt.Errorf("ip link mtu: %w", err)
	}
	if err := runIP("link", "set", "dev", t.name, "up"); err != nil {
		return fmt.Errorf("ip link up: %w", err)
	}
	return nil
}

func runIP(args ...string) error {
	cmd := exec.Command("ip", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip %v: %v: %s", args, err, string(out))
	}
	return nil
}
