package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// IPLease 虚拟 IP 租约:绑定账户/设备并持久化,服务端重启不漂移。
// 按 networkID 隔离:每个账户一个独立虚拟网络,共享同一默认子网地址空间
// (10.0.0.0/24),跨网络不可见(数据面按 networkID+IP 键路由)。
type IPLease struct {
	NetworkID    string    `json:"network_id"`
	OwnerAccount string    `json:"owner_account"`
	OwnerDevice  string    `json:"owner_device"`
	IPv4         string    `json:"ipv4"`
	LeasedAt     time.Time `json:"leased_at"`
}

const defaultCIDR = "10.0.0.0/24"

// netState 单网络内的租约与下一位。
type netState struct {
	Next   net.IP
	Leases map[string]*IPLease // ipv4 -> lease
}

// LeaseStore 网络→租约映射。
type LeaseStore struct {
	mu   sync.RWMutex
	path string
	ipn  *net.IPNet
	nets map[string]*netState
}

// OpenLeases 打开租约库。cidr 为空用默认网段。
func OpenLeases(path string, cidr string) (*LeaseStore, error) {
	if cidr == "" {
		cidr = defaultCIDR
	}
	_, ipn, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse cidr %s: %w", cidr, err)
	}
	ls := &LeaseStore{path: path, ipn: ipn, nets: map[string]*netState{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		var leases []*IPLease
		if err := json.Unmarshal(raw, &leases); err != nil {
			return nil, err
		}
		for _, l := range leases {
			st := ls.getStateLocked(l.NetworkID)
			st.Leases[l.IPv4] = l
		}
		ls.recomputeNextLocked()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return ls, nil
}

// getStateLocked 取/建网络状态(caller 持有锁)。
func (ls *LeaseStore) getStateLocked(networkID string) *netState {
	st, ok := ls.nets[networkID]
	if !ok {
		st = &netState{Next: nextIP(ls.ipn.IP.To4(), 1), Leases: map[string]*IPLease{}}
		ls.nets[networkID] = st
	}
	return st
}

func (ls *LeaseStore) recomputeNextLocked() {
	for _, st := range ls.nets {
		st.Next = nextIP(ls.ipn.IP.To4(), 1)
		// 网络地址的广播地址(最后一个地址)跳过由 Allocate 的 Contains+q检查处理
		if _, taken := st.Leases[st.Next.String()]; !taken {
			continue
		}
		for i := 0; i < 65536; i++ {
			nx := nextIP(st.Next, 1)
			if nx.Equal(st.Next) {
				break
			}
			st.Next = nx
			if _, taken := st.Leases[nx.String()]; !taken {
				break
			}
		}
	}
}

// Allocate 为设备分配(或复用已有)虚拟 IP。
func (ls *LeaseStore) Allocate(networkID, ownerAccount, ownerDevice string) (*IPLease, error) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	st := ls.getStateLocked(networkID)
	for _, l := range st.Leases {
		if l.OwnerDevice == ownerDevice {
			l.OwnerAccount = ownerAccount
			return l, nil
		}
	}
	for i := 0; i < 65536; i++ {
		if !ls.ipn.Contains(st.Next) {
			return nil, fmt.Errorf("network %s exhausted", networkID)
		}
		// 排除广播地址(子网最后一个地址)与网络地址(Next 从不等于网络地址)
		if st.Next.Equal(broadcastAddr(ls.ipn)) {
			st.Next = nextIP(st.Next, 1)
			continue
		}
		ipv4 := st.Next.String()
		st.Next = nextIP(st.Next, 1)
		if _, taken := st.Leases[ipv4]; taken {
			continue
		}
		l := &IPLease{
			NetworkID: networkID, OwnerAccount: ownerAccount,
			OwnerDevice: ownerDevice, IPv4: ipv4, LeasedAt: time.Now(),
		}
		st.Leases[ipv4] = l
		if err := ls.save(); err != nil {
			return nil, err
		}
		return l, nil
	}
	return nil, fmt.Errorf("network %s exhausted", networkID)
}

// Lease 查询单租约(管理面)。
func (ls *LeaseStore) Lease(networkID, deviceID string) (*IPLease, error) {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	st, ok := ls.nets[networkID]
	if !ok {
		return nil, errors.New("no lease")
	}
	for _, l := range st.Leases {
		if l.OwnerDevice == deviceID {
			return l, nil
		}
	}
	return nil, errors.New("no lease")
}

// Array 全部租约。
func (ls *LeaseStore) Array() []*IPLease {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	var out []*IPLease
	for _, st := range ls.nets {
		for _, l := range st.Leases {
			out = append(out, l)
		}
	}
	return out
}

// save 落盘(caller 必须已持锁,Allocate 内保持写锁)。
func (ls *LeaseStore) save() error {
	var leases []*IPLease
	for _, st := range ls.nets {
		for _, l := range st.Leases {
			leases = append(leases, l)
		}
	}
	data, err := json.MarshalIndent(leases, "", "  ")
	if err != nil {
		return err
	}
	tmp := ls.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ls.path)
}

// nextIP 返回 ip+step(原地拷贝,不修改入参)。
func nextIP(ip net.IP, step int) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0 && step > 0; i-- {
		v := int(out[i]) + step
		out[i] = byte(v)
		step = v >> 8
	}
	return out
}

// broadcastAddr 子网广播地址。
func broadcastAddr(n *net.IPNet) net.IP {
	bc := make(net.IP, len(n.IP))
	for i := 0; i < len(n.IP); i++ {
		bc[i] = n.IP[i] | ^n.Mask[i]
	}
	return bc
}
