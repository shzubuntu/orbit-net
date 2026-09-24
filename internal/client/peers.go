package client

import (
	"net"
	"sync"

	"orbit/internal/protocol"
)

// PeerRegistry 在线节点表(不含自己)。
// 维护欢迎列表与 peer_join/peer_leave 广播;按主机名解析出口节点当前虚拟 IP。
type PeerRegistry struct {
	mu   sync.RWMutex
	byIP map[string]protocol.PeerInfo
}

func NewPeerRegistry() *PeerRegistry {
	return &PeerRegistry{byIP: map[string]protocol.PeerInfo{}}
}

func (p *PeerRegistry) Upsert(peer protocol.PeerInfo) {
	if peer.IP == "" {
		return
	}
	p.mu.Lock()
	p.byIP[peer.IP] = peer
	p.mu.Unlock()
}

func (p *PeerRegistry) UpsertMany(peers []protocol.PeerInfo) {
	p.mu.Lock()
	for _, peer := range peers {
		if peer.IP != "" {
			p.byIP[peer.IP] = peer
		}
	}
	p.mu.Unlock()
}

func (p *PeerRegistry) Remove(ip string) {
	p.mu.Lock()
	delete(p.byIP, ip)
	p.mu.Unlock()
}

// Resolve 按主机名返回节点的当前虚拟 IP(v4)。
func (p *PeerRegistry) Resolve(hostname string) (net.IP, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, peer := range p.byIP {
		if peer.Hostname == hostname {
			if ip := net.ParseIP(peer.IP); ip != nil {
				return ip, true
			}
		}
	}
	return nil, false
}

// ResolveEgress 按主机名解析出口节点的虚拟 IP,仅当该节点当前为授权出口(Egress=true)。
func (p *PeerRegistry) ResolveEgress(hostname string) (net.IP, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, peer := range p.byIP {
		if peer.Hostname == hostname && peer.Egress {
			if ip := net.ParseIP(peer.IP); ip != nil {
				return ip, true
			}
		}
	}
	return nil, false
}

// ByIPEgress 判断该虚拟 IP 是否是在线且授权为出口的节点。
func (p *PeerRegistry) ByIPEgress(ip net.IP) bool {
	if ip == nil || ip.To4() == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	peer, ok := p.byIP[ip.To4().String()]
	return ok && peer.Egress
}

func (p *PeerRegistry) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byIP)
}

func (p *PeerRegistry) Snapshot() []protocol.PeerInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]protocol.PeerInfo, 0, len(p.byIP))
	for _, peer := range p.byIP {
		out = append(out, peer)
	}
	return out
}
