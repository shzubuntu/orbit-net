package client

import (
	"log"
	"net"
)

// smartPrefixes smart 模式下需接管系统路由的规则前缀(仅 IPv4)。
func smartPrefixes(c *Client) []*net.IPNet {
	var out []*net.IPNet
	for _, r := range c.routes {
		if r.cidr == nil || r.cidr.IP.To4() == nil {
			continue
		}
		out = append(out, r.cidr)
	}
	return out
}

// coversDefault 前缀是否是 0.0.0.0/0(接管默认路由的语义)。
func coversDefault(p *net.IPNet) bool {
	if p == nil || p.IP.To4() == nil {
		return false
	}
	ones, _ := p.Mask.Size()
	return ones == 0
}

// serverHostIPs 解析配置里的数据面服务器地址, 供 keep-local 直连
// (防控制信道复连时被打回隧道形成回环)。
func serverHostIPs(serverAddr string) []net.IP {
	host := serverAddr
	if h, _, err := net.SplitHostPort(serverAddr); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		log.Printf("orbit: global: resolve %q: %v (skip keep-local)", host, err)
		return nil
	}
	return ips
}
