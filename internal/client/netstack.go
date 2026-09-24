package client

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// dialer 出口节点拨号到真实目标的抽象(平台差异: Linux 需 SO_MARK 防回环)。
type dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// udpIdleTimeout UDP 会话无流量空闲时长,过期自动关闭(防长期泄漏)。
const udpIdleTimeout = 90 * time.Second

// NetStack 出口节点 B 端的 gVisor 用户态协议栈: 接受隧道里的 TCP/UDP,拨号到真实目标。
type NetStack struct {
	s        *stack.Stack
	linkEP   *channel.Endpoint
	onOutput func([]byte) // 响应包回调:调用方重新封装发回 A
	dialer   dialer
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewNetStack 创建并启动 gVisor 协议栈。
// localVMAddr 是本机虚拟 IP(栈绑定的源地址),onOutput 见上,dialer 为 nil 用默认。
func NewNetStack(localVMAddr [4]byte, onOutput func([]byte), d dialer) (*NetStack, error) {
	if d == nil {
		d = newDefaultDialer()
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	})
	linkEP := channel.New(256, 1280, "")

	const nicID tcpip.NICID = 1
	if err := s.CreateNIC(nicID, linkEP); err != nil {
		return nil, fmt.Errorf("CreateNIC: %s", err)
	}
	addr := tcpip.AddrFrom4(localVMAddr)
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 32},
	}, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("AddProtocolAddress: %s", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	// 代理(非真实主机)必须开启 spoofing + promiscuous,否则任意目标 IP 的入站包被丢
	s.SetSpoofing(nicID, true)
	s.SetPromiscuousMode(nicID, true)

	ns := &NetStack{s: s, linkEP: linkEP, onOutput: onOutput, dialer: d}
	fwd := tcp.NewForwarder(s, 0, 1024, ns.handleForwarderRequest)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		ns.handleUDPRequest(r)
		return true
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	ctx, cancel := context.WithCancel(context.Background())
	ns.cancel = cancel
	ns.wg.Add(1)
	go ns.outputLoop(ctx)
	return ns, nil
}

// handleForwarderRequest 每个入站 SYN 一次: 与隧道内流握手,拨号真实目标,双向转发。
func (ns *NetStack) handleForwarderRequest(r *tcp.ForwarderRequest) {
	id := r.ID() // 必须在 Complete 之前取
	targetAddr := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprint(id.LocalPort))

	wq := &waiter.Queue{}
	ep, err := r.CreateEndpoint(wq)
	if err != nil {
		r.Complete(true) // 发 RST
		return
	}
	r.Complete(false)
	clientConn := gonet.NewTCPConn(wq, ep)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target, dialErr := ns.dialer.DialContext(ctx, "tcp", targetAddr)
	if dialErr != nil {
		log.Printf("egress proxy: %s -> %s dial failed: %v", id.RemoteAddress, targetAddr, dialErr)
		_ = clientConn.Close()
		return
	}
	log.Printf("egress proxy: %s -> %s established", id.RemoteAddress, targetAddr)

	ns.wg.Add(2)
	go ns.relay(clientConn, target)
	go ns.relay(target, clientConn)
}

func (ns *NetStack) relay(dst, src net.Conn) {
	defer ns.wg.Done()
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

// handleUDPRequest 每个 UDP 会话请求(远端地址/端口变化即新会话):
// 建 endpoint 拨真目标,双向 datagram 转发,空闲超时自动关闭。
func (ns *NetStack) handleUDPRequest(r *udp.ForwarderRequest) {
	id := r.ID()
	targetAddr := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprint(id.LocalPort))

	wq := &waiter.Queue{}
	ep, err := r.CreateEndpoint(wq)
	if err != nil {
		log.Printf("egress udp: %s create endpoint failed: %v", targetAddr, err)
		return
	}
	inner := gonet.NewUDPConn(wq, ep)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	remote, dialErr := ns.dialer.DialContext(ctx, "udp", targetAddr)
	if dialErr != nil {
		log.Printf("egress udp: %s -> %s dial failed: %v", id.RemoteAddress, targetAddr, dialErr)
		_ = inner.Close()
		return
	}
	log.Printf("egress udp: %s -> %s established", id.RemoteAddress, targetAddr)

	ns.wg.Add(1)
	go ns.relayUDP(inner, remote)
}

// udpFlow 一条 UDP 转发流的双向连接与最后活跃时间。
type udpFlow struct {
	a, b net.Conn
	last time.Time
	mu   sync.Mutex
}

func (f *udpFlow) touch() {
	f.mu.Lock()
	f.last = time.Now()
	f.mu.Unlock()
}

func (f *udpFlow) idleSince() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Since(f.last)
}

// relayUDP 双向 datagram 转发,任一侧出错或空闲超时即收尾(UDp 无半关概念)。
func (ns *NetStack) relayUDP(a, b net.Conn) {
	defer ns.wg.Done()
	defer a.Close()
	defer b.Close()
	flow := &udpFlow{a: a, b: b, last: time.Now()}
	stop := make(chan struct{})
	var once sync.Once
	done := func() { once.Do(func() { close(stop) }) }

	ns.wg.Add(2)
	go ns.copyLoopUDP(a, b, flow, done)
	go ns.copyLoopUDP(b, a, flow, done)

	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if flow.idleSince() > udpIdleTimeout {
				log.Printf("egress udp: idle >%s, closing %s", udpIdleTimeout, a.RemoteAddr())
				return
			}
		}
	}
}

func (ns *NetStack) copyLoopUDP(dst, src net.Conn, f *udpFlow, onDone func()) {
	defer ns.wg.Done()
	buf := make([]byte, 0x10000)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
			f.touch()
		}
		if err != nil {
			break
		}
	}
	onDone()
}

// InjectInbound 把解封装得到的内层 IPv4 包注入 gVisor 入站。
func (ns *NetStack) InjectInbound(ipPacket []byte) {
	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipPacket),
	})
	ns.linkEP.InjectInbound(ipv4.ProtocolNumber, pkb)
}

// outputLoop 读 gVisor 出站响应包,拷贝后回调 onOutput(注意 DecRef 释放)。
func (ns *NetStack) outputLoop(ctx context.Context) {
	defer ns.wg.Done()
	for {
		pkt := ns.linkEP.ReadContext(ctx)
		if pkt == nil {
			return
		}
		ipPacket := pkt.ToView().AsSlice()
		out := make([]byte, len(ipPacket))
		copy(out, ipPacket)
		pkt.DecRef()
		ns.onOutput(out)
	}
}

// Close 关闭协议栈与 outputLoop。
func (ns *NetStack) Close() {
	ns.cancel()
	ns.linkEP.Close()
	ns.s.Close()
	ns.wg.Wait()
}
