//go:build windows

// Package tun Windows 实现: Wintun(官方绑定 golang.zx2c4.com/wintun)。
// 需 exe 同目录存在 wintun.dll; 首次建网卡会经 RUNDLL32 注册驱动, 需管理员权限。
// 安装/自愈计划任务以 SYSTEM 运行(console 子系统 session0 零窗口, 见 AGENTS.md 教训 9)。
package tun

import (
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

type windowsTun struct {
	adapter       *wintun.Adapter
	session       wintun.Session
	sessionActive bool // 仅当 StartSession 成功后才置位; 防止对零值 session 调 End 段错误
	sessionMu     sync.RWMutex
	name          string
	rx            chan []byte
	done          chan struct{}
}

// Create 创建(或重开同名)Wintun 适配器。注意: 会话必须等 Configure(netsh 配网)之后
// 再建立——netsh 设置地址/MTU/启用力会重置接口, 若先 StartSession 会被打断
// (ReceivePacket 报错导致客户端"正常退出"), 见 tun_windows_test 与冒烟记录。
func Create(name string) (Device, error) {
	if name == "" {
		name = "Orbit"
	}
	adapter, err := wintun.CreateAdapter(name, "Orbit", nil)
	if err != nil {
		return nil, fmt.Errorf("wintun CreateAdapter %s: %w", name, err)
	}
	return &windowsTun{
		adapter: adapter,
		name:    name,
		rx:      make(chan []byte, 128),
		done:    make(chan struct{}),
	}, nil
}

// receiveLoop 标准 Wintun 消费模式: 先等服务端 read-wait event, 再排空环形区直到
// ReceivePacket 返回错误(空区 0x259)。该绑定的 ReceivePacket 是非阻塞的, 直接调用
// 在无包时立即返回错误——不先等 event 的话空载就会被误判为出故障退出(实机踩过)。
// 会话操作持有 sessionMu.RLock, 与 Close 的 End 串行化(否则重连 End 后调用
// ReceivePacket 会访问冲突 0xc0000005, 见 reconnect 崩溃记录)。
func (w *windowsTun) receiveLoop() {
	ev := w.session.ReadWaitEvent()
	for {
		// 阻塞等数据事件(1s 超时, 顺便检查退出), 避免 CPU 空转
		if rc, _ := windows.WaitForSingleObject(ev, 1000); rc == windows.WAIT_OBJECT_0 {
			for {
				select {
				case <-w.done:
					return
				default:
				}
				w.sessionMu.RLock()
				select {
				case <-w.done:
					w.sessionMu.RUnlock()
					return
				default:
				}
				pkt, err := w.session.ReceivePacket()
				w.sessionMu.RUnlock()
				if err != nil {
					break // 环形区已空, 回到 wait
				}
				cp := make([]byte, len(pkt))
				copy(cp, pkt)
				w.session.ReleaseReceivePacket(pkt)
				select {
				case w.rx <- cp:
				case <-w.done:
					return
				}
			}
		}
		select {
		case <-w.done:
			return
		default:
		}
	}
}

// Read 从 Wintun 取一个包(阻塞)。
func (w *windowsTun) Read(p []byte) (int, error) {
	select {
	case <-w.done:
		return 0, io.EOF
	case pkt, ok := <-w.rx:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, pkt)
		return n, nil
	}
}

// Write 把包写入 Wintun(进内核协议栈)。会话操作与 Close 的 End 串行化。
func (w *windowsTun) Write(p []byte) (int, error) {
	w.sessionMu.RLock()
	defer w.sessionMu.RUnlock()
	select {
	case <-w.done:
		return 0, io.EOF
	default:
	}
	pkt, err := w.session.AllocateSendPacket(len(p))
	if err != nil {
		return 0, err
	}
	copy(pkt, p)
	w.session.SendPacket(pkt)
	return len(p), nil
}

func (w *windowsTun) Close() error {
	close(w.done)
	if w.sessionActive {
		w.sessionMu.Lock() // 排干在飞的会话操作后再 End, 防访问冲突
		w.session.End()
		w.sessionMu.Unlock()
	}
	return w.adapter.Close()
}

func (w *windowsTun) Name() string { return w.name }

// Configure 用 netsh 配置静态 IP/MTU/启用, 完成后才启动 Wintun 会话并开接收循环。
// 顺序不可颠倒: netsh set address / set subinterface mtu / set interface admin=enable
// 都会(可能)重置适配器; 会话在全部配置完成后建立则不会被中断。
func (w *windowsTun) Configure(ip net.IP, mask net.IPMask, mtu int) error {
	if err := runNetSh("interface", "ip", "set", "address",
		fmt.Sprintf("name=%s", w.name),
		"source=static",
		fmt.Sprintf("addr=%s", ip.String()),
		fmt.Sprintf("mask=%s", net.IP(mask).String()),
		"gateway=none"); err != nil {
		return fmt.Errorf("netsh address: %w", err)
	}
	if err := runNetSh("interface", "ipv4", "set", "subinterface",
		w.name,
		fmt.Sprintf("mtu=%d", mtu),
		"store=persistent"); err != nil {
		return fmt.Errorf("netsh mtu: %w", err)
	}
	// 注意: 不要用 "netsh interface set interface <name> admin=enable"。
	// Wintun 新接口走 NLA 名字解析时该命令常报"此网络连接不存在"(实机翻车),
	// 且适配器创建后本来就处于 enabled。地址+MTU(标准 WireGuard 打法)足够。
	session, err := w.adapter.StartSession(0x400000) // 4M 环形缓冲
	if err != nil {
		return fmt.Errorf("wintun StartSession: %w", err)
	}
	w.session = session
	w.sessionActive = true
	go w.receiveLoop()
	return nil
}

func runNetSh(args ...string) error {
	cmd := exec.Command("netsh", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("netsh %v: %w: %s", args, err, string(out))
	}
	return nil
}
