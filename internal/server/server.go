package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"orbit/internal/account"
	"orbit/internal/protocol"
)

var errServerClosed = errors.New("server closed")

// authTimeout 握手帧读超时。
const authTimeout = 10 * time.Second

// idleTimeout 数据面读超时:超时即判定失联并下线(客户端心跳会刷新)。
const idleTimeout = 90 * time.Second

// Server orbitd 运行时。
type Server struct {
	cfg     Config
	acct    *account.Store
	leases  *account.LeaseStore
	usage   *account.UsageStore
	invites *account.InviteStore
	hub     *Hub
	ln      net.Listener
	lim     *rateLimiter

	mu       sync.Mutex
	closed   bool
	saveCh   chan struct{}
	saveDone chan struct{}
}

// listenTLS 按 TLS 配置监听数据面。
func listenTLS(addr string, tc TLSConfig) (net.Listener, error) {
	cert, err := tls.LoadX509KeyPair(tc.CertFile, tc.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert: %w", err)
	}
	return tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
}

// Serve 阻塞接受数据面连接。
func (s *Server) Serve() error {
	s.saveCh = make(chan struct{})
	s.saveDone = make(chan struct{})
	go s.saveLoop()
	defer func() {
		select { // 停止定期落盘
		case <-s.saveDone:
		default:
			close(s.saveCh)
			<-s.saveDone
		}
		if err := s.usage.Save(); err != nil {
			log.Printf("usage flush on exit: %v", err)
		}
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handleConn(conn)
	}
}

// Close 关闭监听器。
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.ln.Close()
}

// ProtocolVersion 协议版本。
func (s *Server) ProtocolVersion() string { return "0.1" }

// Save 落盘持久化状态。
func (s *Server) Save() error { return s.usage.Save() }

// saveLoop 定期落盘用量(配额/计费唯一事实来源,防重启丢数)。
func (s *Server) saveLoop() {
	defer close(s.saveDone)
	tk := time.NewTicker(60 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-s.saveCh:
			return
		case <-tk.C:
			if err := s.usage.Save(); err != nil {
				log.Printf("usage periodic save: %v", err)
			}
		}
	}
}

// handleConn 一个客户端连接的完整生命周期: 握手 → 数据面转发。
func (s *Server) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	remote := conn.RemoteAddr().String()
	rw := &sysRW{Conn: conn}

	// 握手限流(防爆破,按来源 IP)
	if !s.lim.Allow("hello:"+remote, 10, time.Minute) {
		s.sendReject(rw, "too many connections, slow down")
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(authTimeout))
	typ, payload, err := protocol.ReadFrame(rw, make([]byte, 0, 4096))
	if err != nil {
		return
	}
	if typ != protocol.FrameCtrl {
		return
	}
	msg, err := protocol.DecodeCtrl(payload)
	if err != nil {
		return
	}
	if msg.Type != protocol.MsgHello || msg.Hello == nil {
		s.sendReject(rw, "expected hello")
		return
	}

	// 设备 token 认证 → 账户/设备/虚拟网络
	dev, acct, err := s.authenticate(msg.Hello)
	if err != nil {
		log.Printf("auth fail from %s: %v", remote, err)
		s.sendReject(rw, err.Error())
		return
	}
	networkID := acct.ID // M1: 每账户一个虚拟网络

	welcome, node, err := s.hub.Register(networkID, dev, acct, msg.Hello, rw, remote)
	if err != nil {
		log.Printf("register %s(%s): %v", dev.ID, remote, err)
		s.sendReject(rw, err.Error())
		return
	}

	// 下发 WELCOME(含会话/配额)
	welcomePayload, _ := protocol.EncodeCtrl(&protocol.CtrlMsg{Type: protocol.MsgWelcome, Welcome: welcome})
	if err := protocol.WriteFrame(rw, protocol.FrameCtrl, welcomePayload); err != nil {
		return
	}
	s.acct.MarkSeen(dev)
	log.Printf("device %s (%s) joined net %s as %s egress=%v os=%s",
		dev.ID, remote, welcome.NetworkID, welcome.VirtualIP, node.Egress, msg.Hello.OS)

	node.conn = conn // 挂上底层连接, 供撤销设备时踢下线
	s.hub.NotifyPeerJoined(networkID, node)
	s.dataLoop(rw, node)
}

// authenticate 设备 token 认证(M1 唯一认证模式)。
func (s *Server) authenticate(h *protocol.Hello) (*account.Device, *account.Account, error) {
	if h.Auth.Mode != protocol.AuthDevice {
		return nil, nil, fmt.Errorf("unsupported auth mode: %s", h.Auth.Mode)
	}
	if h.DeviceID == "" || h.Auth.Token == "" {
		return nil, nil, fmt.Errorf("device auth requires device_id and token")
	}
	d, a, err := s.acct.VerifyDevice(h.DeviceID, h.Auth.Token)
	if err != nil {
		return nil, nil, err
	}
	return d, a, nil
}

// dataLoop 数据面: 中继 DATA,响应心跳。
func (s *Server) dataLoop(rw *sysRW, node *Node) {
	defer s.hub.Unregister(node)
	buf := make([]byte, 0, 4096)
	for {
		_ = rw.Conn.SetReadDeadline(time.Now().Add(idleTimeout))
		typ, payload, err := protocol.ReadFrame(rw, buf)
		if err != nil {
			// 正常断开不做日志噪音(EOF/deadline 都是结束信号)
			return
		}
		buf = payload
		switch typ {
		case protocol.FrameData:
			if err := s.hub.Relay(payload, node); err != nil && !isQuotaBlocked(err) {
				log.Printf("relay from %s: %v", node.VirtualIP, err)
			}
		case protocol.FramePing:
			_ = protocol.WriteFrame(rw, protocol.FramePong, nil)
		case protocol.FrameCtrl, protocol.FramePong:
			// 客户端 → 服务端控制面 M1 无;Pong 忽略
		default:
		}
	}
}

// sendReject 发送 REJECT 控制帧。
func (s *Server) sendReject(w io.Writer, reason string) {
	msg := &protocol.CtrlMsg{Type: protocol.MsgReject, Reject: &protocol.Reject{Reason: reason}}
	payload, _ := protocol.EncodeCtrl(msg)
	_ = protocol.WriteFrame(w, protocol.FrameCtrl, payload)
}

// sysRW 线程安全读写包装(数据面写多路:HUB 转发 + 本地应答)。
type sysRW struct {
	net.Conn
	writeMu sync.Mutex
}

func (s *sysRW) Read(p []byte) (int, error) { return s.Conn.Read(p) }
func (s *sysRW) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.Conn.Write(p)
}
