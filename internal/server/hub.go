package server

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"orbit/internal/account"
	"orbit/internal/protocol"
)

// ipProtoIPIP IP-in-IP 封装协议号(IPv4 Protocol 4): 出口代理链路用此判别。
const ipProtoIPIP = 4

// Node 在线节点(一台设备一个)。
type Node struct {
	DeviceID   string
	AccountID  string
	NetworkID  string
	VirtualIP  net.IP
	Hostname   string
	OS         string
	Egress     bool     // 本机开放为出口(hello 上报)
	EgressACL  []string // 允许消费我的账号(空=仅本人)
	JoinedAt   time.Time
	RemoteAddr string
	rw         io.Writer // sysRW,线程安全
	conn       net.Conn  // 底层连接(踢下线用, handleConn 挂载)

	rxBytes atomic.Int64 // 服务器收自该节点的字节数(上行)
	txBytes atomic.Int64 // 服务器转发给该节点的字节数(下行)

	quotaNotified bool // 本次连接是否已发过配额超限通知(每连接一次)
}

// ClientInfo 管理面可见的在线节点快照。
type ClientInfo struct {
	AccountID  string `json:"account_id"`
	DeviceID   string `json:"device_id"`
	NetworkID  string `json:"network_id"`
	VirtualIP  string `json:"virtual_ip"`
	Hostname   string `json:"hostname"`
	OS         string `json:"os"`
	Egress     bool   `json:"egress,omitempty"`
	RemoteAddr string `json:"remote_addr"`
	JoinedAt   string `json:"joined_at"`
	OnlineSecs int64  `json:"online_secs"`
	RxBytes    int64  `json:"rx_bytes"`
	TxBytes    int64  `json:"tx_bytes"`
}

// Network 一个虚拟网络(账户隔离,共享默认网段地址空间)。
type Network struct {
	ID    string
	Nodes map[string]*Node // virtualIP -> Node
}

// netKey 双键路由键: 网络内按目的地 IP 精确路由。
type netKey struct {
	networkID string
	ip        string
}

// Hub 节点注册表 + 中继转发。
type Hub struct {
	mu           sync.Mutex
	cidr         string
	defaultQuota QuotaSpec
	// tiers 计费档位表 + acct 账户库(启用分档配额时非空, M3)。空 = 全账户统一 defaultQuota。
	tiers       map[string]QuotaSpec
	acct        *account.Store
	quota       *quotaTrk
	leases      *account.LeaseStore
	usage       *account.UsageStore
	ipTable     map[netKey]*Node
	networks    map[string]*Network
	netIPCached *net.IPNet
}

// NewHub 构造 Hub。
func NewHub(cidr string, q QuotaSpec, leases *account.LeaseStore, usage *account.UsageStore) *Hub {
	return &Hub{
		cidr:         cidr,
		defaultQuota: q,
		quota:        newQuotaTrk(q, usage, leases),
		leases:       leases,
		usage:        usage,
		ipTable:      make(map[netKey]*Node),
		networks:     make(map[string]*Network),
	}
}

// SetTierQuota 启用分档配额。tiers 为 nil 表示不启用, 全账户回落 defaultQuota。
// 仅在构造阶段调用(welcome/notice 与数据面转发都经此解析)。
func (h *Hub) SetTierQuota(tiers map[string]QuotaSpec, acct *account.Store) {
	h.tiers = tiers
	h.acct = acct
	if h.quota != nil {
		h.quota.enableTiers(tiers, acct)
	}
}

// quotaSpecFor 账户档位配额解析(Hub 侧, 与 Config.QuotaFor 同源规则)。
func (h *Hub) quotaSpecFor(accountID string) QuotaSpec {
	if h.tiers == nil || h.acct == nil {
		return h.defaultQuota
	}
	if a, err := h.acct.Account(accountID); err == nil {
		if sp, ok := h.tiers[a.Tier]; ok {
			return sp
		}
	}
	return h.defaultQuota
}

// Register 分配 IP、登记节点、组装 Welcome(含同网在线节点与会话/配额)。
// 由调用方负责发送 WELCOME 与广播 PEER_JOIN。
func (h *Hub) Register(networkID string, dev *account.Device, acct *account.Account,
	hello *protocol.Hello, rw io.Writer, remoteAddr string) (*protocol.Welcome, *Node, error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	lease, err := h.leases.Allocate(networkID, acct.ID, dev.ID)
	if err != nil {
		return nil, nil, err
	}
	ip := net.ParseIP(lease.IPv4)
	key := netKey{networkID, ip.String()}

	// 同设备重复连接:顶掉旧 Node(防 ipTable 残留)
	if old, ok := h.ipTable[key]; ok && old.DeviceID == dev.ID {
		h.removeLocked(old)
	}

	node := &Node{
		DeviceID: dev.ID, AccountID: acct.ID, NetworkID: networkID,
		VirtualIP: ip, Hostname: hello.Hostname, OS: hello.OS,
		// 出口开关以 store 的 EgressEnabled 为权威(客户端的 egress:true 只是申请,必须过审)
		Egress: hello.Egress && dev.EgressEnabled, EgressACL: dev.EgressACL,
		JoinedAt: time.Now(), RemoteAddr: remoteAddr, rw: rw,
	}

	net, ok := h.networks[networkID]
	if !ok {
		net = &Network{ID: networkID, Nodes: map[string]*Node{}}
		h.networks[networkID] = net
	}
	net.Nodes[ip.String()] = node
	h.ipTable[key] = node

	// 同网在线节点列表
	peers := make([]protocol.PeerInfo, 0, len(net.Nodes)-1)
	for _, p := range net.Nodes {
		if p.VirtualIP.Equal(ip) {
			continue
		}
		peers = append(peers, protocol.PeerInfo{
			IP: p.VirtualIP.String(), Hostname: p.Hostname, OS: p.OS, Egress: p.Egress,
		})
	}

	welcome := &protocol.Welcome{
		VirtualIP: ip.String(),
		CIDR:      h.cidr,
		MTU:       1280,
		NetworkID: networkID,
		Peers:     peers,
		Session:   h.composeSession(acct, dev),
	}
	return welcome, node, nil
}

// KickDevice 撤销设备后踢下线: 关闭其底层连接, 由断开流程摘节点+广播离开。
func (h *Hub) KickDevice(deviceID string) bool {
	h.mu.Lock()
	var conn net.Conn
	for _, n := range h.ipTable {
		if n.DeviceID == deviceID {
			conn = n.conn
			break
		}
	}
	h.mu.Unlock()
	if conn == nil {
		return false
	}
	_ = conn.Close()
	return true
}

// UpdateEgress 热更新在线节点的出口授权(enabled + ACL), 无需重连立即生效。
func (h *Hub) UpdateEgress(deviceID string, enabled bool, acl []string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.ipTable {
		if n.DeviceID == deviceID {
			n.Egress = enabled
			n.EgressACL = acl
			return true
		}
	}
	return false
}

// composeSession 组装会话摘要(含配额用量; 限额按账户档位解析)。
func (h *Hub) composeSession(acct *account.Account, dev *account.Device) *protocol.Session {
	since := time.Now().Truncate(24 * time.Hour)
	t := h.usage.Totals(acct.ID, since)
	e := h.usage.EgressTotals(dev.ID, since)
	spec := h.quotaSpecFor(acct.ID)
	return &protocol.Session{
		AccountID: acct.ID,
		DeviceID:  dev.ID,
		Tier:      acct.Tier,
		Quota: &protocol.Quota{
			Period:           "day",
			UsedBytes:        t.RxBytes + t.TxBytes,
			LimitBytes:       spec.BytesPerDay,
			EgressUsedBytes:  e.RxBytes + e.TxBytes,
			EgressLimitBytes: spec.EgressBytesPerDay,
		},
	}
}

// NotifyPeerJoined 广播 PEER_JOIN(在 Register 的锁释放后调用)。
func (h *Hub) NotifyPeerJoined(networkID string, node *Node) {
	h.broadcastCtrl(networkID, &protocol.CtrlMsg{
		Type: protocol.MsgPeerJoin,
		PeerJoin: &protocol.PeerInfo{
			IP: node.VirtualIP.String(), Hostname: node.Hostname, OS: node.OS, Egress: node.Egress,
		},
	}, node.VirtualIP)
}

// Unregister 下线清理 + 广播 PEER_LEAVE。
func (h *Hub) Unregister(node *Node) {
	if node == nil {
		return
	}
	h.mu.Lock()
	h.removeLocked(node)
	h.mu.Unlock()
	h.broadcastCtrl(node.NetworkID, &protocol.CtrlMsg{
		Type:      protocol.MsgPeerLeave,
		PeerLeave: &protocol.PeerLeave{IP: node.VirtualIP.String()},
	}, node.VirtualIP)
}

// removeLocked 从各表移除节点(caller 持有锁)。
func (h *Hub) removeLocked(node *Node) {
	key := netKey{node.NetworkID, node.VirtualIP.String()}
	if cur, ok := h.ipTable[key]; ok && cur == node {
		delete(h.ipTable, key)
	}
	if net, ok := h.networks[node.NetworkID]; ok {
		if cur, ok := net.Nodes[node.VirtualIP.String()]; ok && cur == node {
			delete(net.Nodes, node.VirtualIP.String())
		}
	}
}

// Relay 中继一个 DATA 帧到目标节点,并累计用量。
// 目标 = 包目的 IP 在源节点所在网络内映射的节点(含出口链路的"外层 dst=出口节点")。
func (h *Hub) Relay(packet []byte, src *Node) error {
	if len(packet) < 20 {
		return fmt.Errorf("invalid ip packet")
	}
	dst := net.IPv4(packet[16], packet[17], packet[18], packet[19])

	h.mu.Lock()
	dstNode, ok := h.ipTable[netKey{src.NetworkID, dst.String()}]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("no route to %s in net %s", dst, src.NetworkID)
	}
	// 出口 ACL: 目标开放为出口时,消费方须同属于本账户或在 ACL 白名单
	if dstNode.Egress && dstNode.AccountID != src.AccountID && !aclAllow(dstNode.EgressACL, src.AccountID) {
		h.mu.Unlock()
		return fmt.Errorf("egress denied: %s cannot use %s", src.AccountID, dstNode.DeviceID)
	}
	rw := dstNode.rw
	h.mu.Unlock()

	// 计费归因(发送方计费 + 出口代理双向归消费者)。
	// 判别: 外层 IP 协议号 = 4(IP-in-IP 封装)= 代理链路; 单层头 = 直达 mesh。
	//  - 消费者→出口(带壳): 记消费者, 出口泳道, 桶第三段=出口节点;
	//  - 出口→消费者(带壳回包): 改记接收方消费者(账户/设备), 出口泳道,
	//    桶第三段=出口节点 —— 使出口配额真正能挡下载, 出口盒不再背下新月流量;
	//  - 直达出口盒的 mesh 包(无壳): 一律普通流量, 不入出口泳道(修旧版错账)。
	proxied := len(packet) >= 20+20 && packet[9] == ipProtoIPIP
	accID, devID, egrDev, egrPath := src.AccountID, src.DeviceID, "", false
	switch {
	case dstNode.Egress:
		egrPath = proxied
		if egrPath {
			egrDev = dstNode.DeviceID
		}
	case src.Egress && proxied:
		accID, devID = dstNode.AccountID, dstNode.DeviceID
		egrPath, egrDev = true, src.DeviceID
	}

	// 配额硬限(QuotaEnforcer): 账户日流量 / 消费者出口日流量, 超限拒发 + 每连接一次通知。
	if err := h.quota.admit(accID, devID, egrPath, int64(len(packet))); err != nil {
		if !src.quotaNotified {
			src.quotaNotified = true
			h.sendQuotaNotice(src)
		}
		return err
	}

	hdr := [3]byte{protocol.FrameData}
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(packet)))
	if _, err := rw.Write(hdr[:]); err != nil {
		return fmt.Errorf("relay header: %w", err)
	}
	if _, err := rw.Write(packet); err != nil {
		return fmt.Errorf("relay payload: %w", err)
	}

	src.rxBytes.Add(int64(len(packet)))
	dstNode.txBytes.Add(int64(len(packet)))
	h.quota.inc(accID, devID, egrPath, int64(len(packet)))
	// 用量单计(与 quota.inc 同源): 每成功转发一包, 记归属桶 tx=包长一次;
	// rx 恒 0。旧版 rx/tx 双写同一包导致 ledger/重启回填 2 倍, 已修。
	h.usage.Add(accID, devID, egrDev, 0, int64(len(packet)), time.Now())
	return nil
}

// sendQuotaNotice 给节点发一次 quota 控制帧(服务器中枢当前用量+限额, 供客户端提示)。
func (h *Hub) sendQuotaNotice(node *Node) {
	since := time.Now().Truncate(24 * time.Hour)
	t := h.usage.Totals(node.AccountID, since)
	e := h.usage.EgressTotals(node.DeviceID, since)
	spec := h.quotaSpecFor(node.AccountID)
	msg := &protocol.CtrlMsg{
		Type: protocol.MsgQuota,
		Quota: &protocol.Quota{
			Period:           "day",
			UsedBytes:        t.RxBytes + t.TxBytes,
			LimitBytes:       spec.BytesPerDay,
			EgressUsedBytes:  e.RxBytes + e.TxBytes,
			EgressLimitBytes: spec.EgressBytesPerDay,
		},
	}
	payload, err := protocol.EncodeCtrl(msg)
	if err != nil {
		return
	}
	if err := protocol.WriteFrame(node.rw, protocol.FrameCtrl, payload); err != nil {
		log.Printf("send quota notice to %s: %v", node.VirtualIP, err)
	}
}

// QuotaNotifyAccount 档位切换后向该账户所有在线节点推送最新配额(无需重连立即生效)。
func (h *Hub) QuotaNotifyAccount(accountID string) {
	h.mu.Lock()
	var nodes []*Node
	for _, n := range h.ipTable {
		if n.AccountID == accountID {
			nodes = append(nodes, n)
		}
	}
	h.mu.Unlock()
	for _, n := range nodes {
		h.sendQuotaNotice(n)
	}
}

// aclAllow 白名单判断(空白名单 = 仅本人,即不允许跨账户)。
func aclAllow(acl []string, accountID string) bool {
	for _, id := range acl {
		if id == accountID {
			return true
		}
	}
	return false
}

// broadcastCtrl 向网内所有除 excludeIP 外的成员发控制帧。
func (h *Hub) broadcastCtrl(networkID string, msg *protocol.CtrlMsg, excludeIP net.IP) {
	payload, err := protocol.EncodeCtrl(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	net, ok := h.networks[networkID]
	if !ok {
		h.mu.Unlock()
		return
	}
	var targets []io.Writer
	for _, n := range net.Nodes {
		if excludeIP != nil && n.VirtualIP.Equal(excludeIP) {
			continue
		}
		targets = append(targets, n.rw)
	}
	h.mu.Unlock()
	for _, w := range targets {
		if err := protocol.WriteFrame(w, protocol.FrameCtrl, payload); err != nil {
			log.Printf("broadcast to peer: %v", err)
		}
	}
}

// ListClients 在线节点快照(管理面)。
func (h *Hub) ListClients() []ClientInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]ClientInfo, 0, len(h.ipTable))
	for _, n := range h.ipTable {
		out = append(out, ClientInfo{
			AccountID: n.AccountID, DeviceID: n.DeviceID, NetworkID: n.NetworkID,
			VirtualIP: n.VirtualIP.String(), Hostname: n.Hostname, OS: n.OS,
			Egress: n.Egress, RemoteAddr: n.RemoteAddr,
			JoinedAt:   n.JoinedAt.Format("2006-01-02 15:04:05"),
			OnlineSecs: int64(now.Sub(n.JoinedAt).Seconds()),
			RxBytes:    n.rxBytes.Load(), TxBytes: n.txBytes.Load(),
		})
	}
	return out
}
