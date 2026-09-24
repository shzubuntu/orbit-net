package client

import (
	"encoding/binary"
	"fmt"
	"net"
)

// IP-in-IP 封装(RFC 2003,协议号 4)。
// A 端:命中出口规则的包套外层头(外层 dst=出口节点虚拟 IP),服务器按外层 dst 中继;
// B 端:收到 protocol=4 且 dst=自己虚拟 IP 的外层包 → 解封装注入 gVisor netstack。

const (
	ipProtoIPIP = 4
	ipv4HdrLen  = 20
)

// routeEntry 编译后的单条智能模式出口规则。
type routeEntry struct {
	cidr *net.IPNet
	via  string // 出口设备主机名或虚拟 IP(主机名在发包时实时解析)
}

// buildRoutes 把配置的 RuleSpec 编译为路由表。
func buildRoutes(specs []RuleSpec) ([]routeEntry, error) {
	var routes []routeEntry
	for _, s := range specs {
		if s.ExitDevice == "" {
			return nil, fmt.Errorf("rule %s: exit_device required", s.Target)
		}
		var cidr *net.IPNet
		if ip := net.ParseIP(s.Target); ip != nil {
			ones := 32
			if ip.To4() == nil {
				ones = 128
			}
			cidr = &net.IPNet{IP: ip, Mask: net.CIDRMask(ones, 32)}
		} else {
			var err error
			_, cidr, err = net.ParseCIDR(s.Target)
			if err != nil {
				return nil, fmt.Errorf("parse target %q: %w", s.Target, err)
			}
		}
		routes = append(routes, routeEntry{cidr: cidr, via: s.ExitDevice})
	}
	return routes, nil
}

// matchRoute 查出口规则,返回命中的 via(出口设备主机名/IP),未命中返回空。
func (c *Client) matchRoute(dst net.IP) (string, bool) {
	dst4 := dst.To4()
	if dst4 == nil {
		return "", false
	}
	for _, r := range c.routes {
		if r.cidr.Contains(dst4) {
			return r.via, true
		}
	}
	return "", false
}

// resolveVia 解析出口设备(via)的当前虚拟 IP。
// 仅接受"在线且当前被授权为出口(Egress=true)"的节点:
//   - IP 形式: 该 IP 必须在在线节点表且 Egress=true;
//   - 主机名形式: 在在线节点表中命中主机名且 Egress=true。
//
// 出口被 admin 吊销(join 时 Egress=false)后,消费方将无法再经由它路由,与中继侧管控一致。
func (c *Client) resolveVia(via string) net.IP {
	if ip := net.ParseIP(via); ip != nil && ip.To4() != nil {
		if c.peers.ByIPEgress(ip) {
			return ip
		}
		return nil
	}
	if ip, ok := c.peers.ResolveEgress(via); ok {
		return ip
	}
	return nil
}

// globalExit global 模式的默认出口: 显式 exit_device 优先,否则取在线首台 egress 节点。
func (c *Client) globalExit() string {
	if c.cfg.ExitDevice != "" {
		return c.cfg.ExitDevice
	}
	for _, p := range c.peers.Snapshot() {
		if p.Egress {
			return p.Hostname
		}
	}
	return ""
}

// isLocalVirtual dst 是否属于本机虚拟网段(网段内设备直连, 不走出口封装)。
func (c *Client) isLocalVirtual(dst net.IP) bool {
	dst4 := dst.To4()
	loc4 := c.localIP.To4()
	if dst4 == nil || loc4 == nil || c.mask == nil {
		return false
	}
	net4 := append(net.IP(nil), loc4...)
	for i := 0; i < 4; i++ {
		net4[i] &= c.mask[i]
	}
	dstNet := append(net.IP(nil), dst4...)
	for i := 0; i < 4; i++ {
		dstNet[i] &= c.mask[i]
	}
	return net4.Equal(dstNet)
}

// encapsulate 把 inner 包封装为 IP-in-IP 到 out,返回长度。调用方保证 out 容量足够。
func encapsulate(inner []byte, localIP, via net.IP, out []byte) int {
	out[0] = 0x45
	out[1] = inner[1]
	binary.BigEndian.PutUint16(out[2:4], uint16(ipv4HdrLen+len(inner)))
	binary.BigEndian.PutUint16(out[4:6], 0)
	binary.BigEndian.PutUint16(out[6:8], 0)
	out[8] = 64
	out[9] = ipProtoIPIP
	binary.BigEndian.PutUint16(out[10:12], 0)
	copy(out[12:16], localIP.To4())
	copy(out[16:20], via.To4())
	binary.BigEndian.PutUint16(out[10:12], ipChecksum(out[:ipv4HdrLen]))
	copy(out[ipv4HdrLen:], inner)
	return ipv4HdrLen + len(inner)
}

// decapsulate 检测并解封装 IP-in-IP 包(外层 dst=本机虚拟 IP 才剥壳)。
func decapsulate(packet []byte, localIP net.IP) []byte {
	if len(packet) < ipv4HdrLen || packet[0]>>4 != 4 || packet[9] != ipProtoIPIP {
		return nil
	}
	dst := net.IPv4(packet[16], packet[17], packet[18], packet[19])
	if !dst.Equal(localIP) {
		return nil
	}
	return packet[ipv4HdrLen:]
}

// parseDst 取 IPv4 包头目的地址。
func parseDst(packet []byte) net.IP {
	if len(packet) < 20 {
		return nil
	}
	return net.IPv4(packet[16], packet[17], packet[18], packet[19])
}

// ipChecksum IPv4 头校验和。
func ipChecksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(hdr); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(hdr[i : i+2]))
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
