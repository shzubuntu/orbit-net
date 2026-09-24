package client

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"orbit/internal/protocol"
	"orbit/internal/rollinglog"
	"orbit/internal/tun"
)

// Client orbit-cli 运行时: 设备握手 → TUN → 数据面桥接 + 智能模式出口。
type Client struct {
	cfg     *Config
	conn    net.Conn
	writeMu sync.Mutex

	localIP net.IP
	mask    net.IPMask
	mtu     int

	peers  *PeerRegistry
	routes []routeEntry // 智能/全局模式出口规则(Target→via)

	tunDev tun.Device
	ns     *NetStack // 本机开放为出口时非 nil

	routeCleanup func() // 全局模式接管默认路由后的恢复函数

	closeMu sync.Mutex
	closed  bool
	closeCh chan struct{}
	readyCh chan struct{}
	wg      sync.WaitGroup
}

// New 构造客户端。
func New(cfg *Config) (*Client, error) {
	routes, err := buildRoutes(cfg.Rules)
	if err != nil {
		return nil, err
	}
	return &Client{
		cfg:     cfg,
		peers:   NewPeerRegistry(),
		routes:  routes,
		closeCh: make(chan struct{}),
		readyCh: make(chan struct{}),
	}, nil
}

// ReadyCh 就绪信号(握手 + TUN 配置完成)。
func (c *Client) ReadyCh() <-chan struct{} { return c.readyCh }

// VirtualIP 当前分配到的虚拟 IP。
func (c *Client) VirtualIP() net.IP { return c.localIP }

// Peers 在线节点快照(不含自己)。
func (c *Client) Peers() []protocol.PeerInfo { return c.peers.Snapshot() }

// Run 主流程: 拨号握手 → 开 TUN → 桥接 → 阻塞。可重复调用(重连时自动复位)。
func (c *Client) Run() error {
	c.closeMu.Lock()
	c.closed = false
	c.closeCh = make(chan struct{})
	c.readyCh = make(chan struct{})
	c.conn = nil
	c.closeMu.Unlock()

	conn, welcome, err := dialAndAuth(c.cfg)
	if err != nil {
		return err
	}
	c.conn = conn
	c.localIP = net.ParseIP(welcome.VirtualIP)
	if c.localIP == nil {
		_ = conn.Close()
		return fmt.Errorf("invalid virtual_ip: %s", welcome.VirtualIP)
	}
	c.mtu = welcome.MTU
	if c.mtu == 0 {
		c.mtu = 1280
	}
	_, ipNet, err := net.ParseCIDR(welcome.CIDR)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("parse cidr %s: %w", welcome.CIDR, err)
	}
	c.mask = ipNet.Mask
	log.Printf("orbit: got virtual IP %s on %s (net %s)", c.localIP, welcome.CIDR, welcome.NetworkID)
	if welcome.Session != nil {
		q := welcome.Session.Quota
		if q != nil && q.LimitBytes > 0 {
			log.Printf("orbit: quota used %d/%d bytes (day), egress used %d/%d",
				q.UsedBytes, q.LimitBytes, q.EgressUsedBytes, q.EgressLimitBytes)
		}
	}

	tunDev, err := tun.Create(c.cfg.TunName)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("create tun: %w", err)
	}
	c.tunDev = tunDev
	if err := tunDev.Configure(c.localIP, c.mask, c.mtu); err != nil {
		_ = tunDev.Close()
		_ = conn.Close()
		return fmt.Errorf("configure tun: %w", err)
	}
	log.Printf("orbit: tun %s configured %s/%d mtu %d", tunDev.Name(), c.localIP, maskOnes(c.mask), c.mtu)

	if c.cfg.Mode == protocol.ModeGlobal {
		cleanupRoutes, err := applyGlobalRoutes(c)
		if err != nil {
			_ = tunDev.Close()
			_ = conn.Close()
			return fmt.Errorf("apply global routes: %w", err)
		}
		c.routeCleanup = cleanupRoutes
		log.Printf("orbit: global mode: default route -> %s (keep-local %d)", tunDev.Name(), len(c.cfg.KeepLocal)+1)
	} else if c.cfg.Mode == protocol.ModeSmart {
		cleanupRoutes, err := applySmartRoutes(c)
		if err != nil {
			_ = tunDev.Close()
			_ = conn.Close()
			return fmt.Errorf("apply smart routes: %w", err)
		}
		c.routeCleanup = cleanupRoutes
		log.Printf("orbit: smart mode: %d capture route(s) -> %s", len(smartPrefixes(c)), tunDev.Name())
	}

	// 在线节点 + 出口链路由
	c.peers.UpsertMany(welcome.Peers)
	if c.cfg.Egress {
		var lb [4]byte
		copy(lb[:], c.localIP.To4())
		ns, err := NewNetStack(lb, c.onNetStackOutput, nil)
		if err != nil {
			_ = tunDev.Close()
			_ = conn.Close()
			return fmt.Errorf("create netstack: %w", err)
		}
		c.ns = ns
		log.Printf("orbit: egress mode on, netstack bound to %s", c.localIP)
	}
	log.Printf("orbit: online peers %d, egress rules %d", c.peers.Count(), len(c.routes))

	c.wg.Add(3)
	go c.recvLoop()
	go c.tunToServer()
	go c.pingLoop()

	close(c.readyCh)
	<-c.closeCh
	c.cleanup()
	return nil
}

// sendData 把 IP 包作为 DATA 帧发给服务器(并发安全 + 写超时)。
func (c *Client) sendData(payload []byte) error {
	if c.closed {
		return errors.New("client closed")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if ds, ok := c.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = ds.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer func() { _ = ds.SetWriteDeadline(time.Time{}) }()
	}
	return protocol.WriteFrame(c.conn, protocol.FrameData, payload)
}

// recvLoop 从服务器读帧并分发。
func (c *Client) recvLoop() {
	defer c.wg.Done()
	buf := make([]byte, 0, 65536)
	for {
		typ, payload, err := protocol.ReadFrame(c.conn, buf)
		if err != nil {
			c.markClosed(err)
			return
		}
		buf = payload
		switch typ {
		case protocol.FrameData:
			c.handleInbound(payload)
		case protocol.FrameCtrl:
			c.handleCtrl(payload)
		case protocol.FramePing:
			c.writeMu.Lock()
			_ = protocol.WriteFrame(c.conn, protocol.FramePong, nil)
			c.writeMu.Unlock()
		case protocol.FramePong:
		}
	}
}

// handleInbound 处理服务器转发来的 IP 包: 出口封装交给 netstack,其余喂 TUN。
func (c *Client) handleInbound(packet []byte) {
	if inner := decapsulate(packet, c.localIP); inner != nil {
		if c.ns != nil {
			// ICMP echo 在出口侧直接回,不占 gVisor 会话
			if r := icmpEchoReply(inner); r != nil {
				c.onNetStackOutput(r)
				return
			}
			c.ns.InjectInbound(inner)
		} else {
			if _, err := c.tunDev.Write(inner); err != nil {
				c.markClosed(err)
			}
		}
		return
	}
	if _, err := c.tunDev.Write(packet); err != nil {
		c.markClosed(err)
	}
}

// handleCtrl 处理服务器控制帧(在线节点维护)。
func (c *Client) handleCtrl(payload []byte) {
	m, err := protocol.DecodeCtrl(payload)
	if err != nil {
		log.Printf("orbit: bad ctrl frame: %v", err)
		return
	}
	switch m.Type {
	case protocol.MsgPeerJoin:
		if m.PeerJoin != nil {
			log.Printf("orbit: peer joined %s (%s) egress=%v", m.PeerJoin.IP, m.PeerJoin.Hostname, m.PeerJoin.Egress)
			c.peers.Upsert(*m.PeerJoin)
		}
	case protocol.MsgPeerLeave:
		if m.PeerLeave != nil {
			log.Printf("orbit: peer left %s", m.PeerLeave.IP)
			c.peers.Remove(m.PeerLeave.IP)
		}
	case protocol.MsgQuota:
		if m.Quota != nil {
			log.Printf("orbit: quota hard-stop: used %d/%d bytes (egress %d/%d); server blocked further traffic until quota resets",
				m.Quota.UsedBytes, m.Quota.LimitBytes, m.Quota.EgressUsedBytes, m.Quota.EgressLimitBytes)
		}
	}
}

// tunToServer TUN → 服务器: 出口规则命中则 IP-in-IP 封装(防直连泄漏),否则原样发送。
func (c *Client) tunToServer() {
	defer c.wg.Done()
	buf := make([]byte, c.mtu+4)
	encBuf := make([]byte, c.mtu+4+ipv4HdrLen)
	for {
		n, err := c.tunDev.Read(buf)
		if err != nil {
			c.markClosed(err)
			return
		}
		if n < 20 || buf[0]>>4 != 4 {
			continue
		}
		pkt := buf[:n]
		var send []byte
		if dst := parseDst(pkt); dst != nil && !c.isLocalVirtual(dst) {
			// 虚拟子网内流量(对应 mesh 对端)永远走控制信道直达, 不做出口封装,
			// 防止规则 0.0.0.0/0 把指向对端节点的包经出口设备转一圈打回自己。
			via, ok := c.matchRoute(dst)
			if !ok && c.cfg.Mode == protocol.ModeGlobal {
				// global 模式: 网段外的流量喂默认出口
				via, ok = c.globalExit(), true
			}
			if ok {
				vip := c.resolveVia(via)
				if vip == nil {
					continue // 出口设备离线: 丢弃,绝不能走普通路径直连泄漏
				}
				en := encapsulate(pkt, c.localIP, vip, encBuf)
				send = encBuf[:en]
			}
		}
		if send == nil {
			send = pkt
		}
		if err := c.sendData(send); err != nil {
			c.markClosed(err)
			return
		}
	}
}

// onNetStackOutput B 端出口: gVisor 响应包重新封装(外层 src=本机,dst=内层目标)发回 A。
func (c *Client) onNetStackOutput(ipPacket []byte) {
	dst := parseDst(ipPacket)
	if dst == nil {
		return
	}
	out := make([]byte, len(ipPacket)+ipv4HdrLen)
	n := encapsulate(ipPacket, c.localIP, dst, out)
	if err := c.sendData(out[:n]); err != nil {
		log.Printf("orbit: egress resp send: %v", err)
	}
}

// pingLoop 周期心跳,保持连接与等待配额/节点推送。
func (c *Client) pingLoop() {
	defer c.wg.Done()
	tk := time.NewTicker(30 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-c.closeCh:
			return
		case <-tk.C:
			c.writeMu.Lock()
			_ = protocol.WriteFrame(c.conn, protocol.FramePing, nil)
			c.writeMu.Unlock()
		}
	}
}

// Close 外部触发退出。
func (c *Client) Close() error { c.markClosed(nil); return nil }

func (c *Client) markClosed(_ error) {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.closeCh)
}

// cleanup 必须先关资源再 wg.Wait(顺序: 路由 → TUN → netstack → 连接)。
func (c *Client) cleanup() {
	if c.routeCleanup != nil {
		c.routeCleanup()
		c.routeCleanup = nil
	}
	if c.tunDev != nil {
		if err := c.tunDev.Close(); err != nil {
			log.Printf("orbit: cleanup tun close err=%v", err)
		}
	}
	if c.ns != nil {
		c.ns.Close()
	}
	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			log.Printf("orbit: cleanup conn close err=%v", err)
		}
	}
	waitDone := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		log.Printf("orbit: cleanup complete")
	case <-time.After(3 * time.Second):
		log.Printf("orbit: cleanup wg.Wait timeout, force returning")
	}
}

func maskOnes(m net.IPMask) int {
	ones, _ := m.Size()
	return ones
}

// SetupLog 配置滚动日志(日志文件非空时)。
func SetupLog(cfg *Config) (*rollinglog.Manager, error) {
	if cfg.LogFile == "" {
		return nil, nil
	}
	return rollinglog.Setup(cfg.LogFile, cfg.LogMaxBytes, cfg.LogKeep)
}
