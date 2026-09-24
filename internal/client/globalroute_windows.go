//go:build windows

package client

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
)

// psRun 执行一段 PowerShell, 返回输出; 失败返回包含输出的错误。
func psRun(script string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("powershell: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// routeCIDR 把 keep-local 条目归一为前缀 CIDR(单 IP 按 /32)。
func routeCIDR(spec string) string {
	spec = strings.TrimSpace(spec)
	if ip := net.ParseIP(spec); ip != nil {
		if ip.To4() != nil {
			return ip.String() + "/32"
		}
		return ""
	}
	if _, ipNet, err := net.ParseCIDR(spec); err == nil && ipNet.IP.To4() != nil {
		return ipNet.String()
	}
	return ""
}

// findTunIndex 以虚拟 IP 反查 TUN 接口索引。
func findTunIndex(c *Client) (string, error) {
	out, err := psRun("$a=Get-NetIPAddress -IPAddress '" + c.localIP.String() + "' -ErrorAction SilentlyContinue; if(-not $a){'ERR:no-tun-iface'; exit 1}; $a.InterfaceIndex")
	if err != nil {
		return "", err
	}
	for _, f := range strings.Fields(out) {
		if f != "ERR:no-tun-iface" {
			return f, nil
		}
	}
	return "", fmt.Errorf("tun iface index empty: %s", out)
}

// applyGlobalRoutes 全局模式: 接管默认路由到 TUN, 服务器/keep-local 保留直连。
// 用 New-NetRoute 显式绑定接口索引(route.exe 的网关接口解析会把 next-hop 错绑到 WiFi,
// 且 metric 语义按 接口Metric+路由Metric 合并算成本, 必须绑定 TUN 接口才能压过原默认路由)。
// 删除只按 prefix+接口匹配: on-link 默认路由在系统中网关归一化为 0.0.0.0, 按 NextHop 删会漏。
func applyGlobalRoutes(c *Client) (func(), error) {
	tunIP := c.localIP.String()

	// 1. TUN 接口索引: 以虚拟 IP 反查。
	tunIdx, err := findTunIndex(c)
	if err != nil {
		return nil, fmt.Errorf("find tun iface: %w", err)
	}

	// 2. 原有默认路由(网关+接口), keep-local 沿它保持直连。
	gw, gwIdx := "", ""
	if out, err := psRun("$r=Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue | Where-Object {$_.NextHop.ToString() -ne '" + tunIP + "'} | Select-Object -First 1; if($r){$r.NextHop.ToString()+' '+$r.InterfaceIndex; exit 0}; exit 2"); err == nil {
		f := strings.Fields(out)
		if len(f) >= 2 {
			gw, gwIdx = f[0], f[1]
		}
	}

	type op struct{ add, del string }
	var ops []op

	if gw != "" && gwIdx != "" {
		keep := append([]string{}, c.cfg.KeepLocal...)
		for _, ip := range serverHostIPs(c.cfg.ServerAddr) {
			keep = append(keep, ip.String())
		}
		for _, spec := range keep {
			prefix := routeCIDR(spec)
			if prefix == "" {
				log.Printf("orbit: global: skip keep-local %q", spec)
				continue
			}
			add := "New-NetRoute -DestinationPrefix '" + prefix + "' -NextHop '" + gw + "' -InterfaceIndex " + gwIdx + " -RouteMetric 1 -PolicyStore ActiveStore -Confirm:$false | Out-Null"
			del := "Remove-NetRoute -DestinationPrefix '" + prefix + "' -InterfaceIndex " + gwIdx + " -Confirm:$false -ErrorAction SilentlyContinue"
			ops = append(ops, op{add, del})
		}
	}

	// 3. 默认路由显式绑 TUN 接口, 全流量进隧道。
	ops = append(ops, op{
		add: "New-NetRoute -DestinationPrefix '0.0.0.0/0' -NextHop '" + tunIP + "' -InterfaceIndex " + tunIdx + " -RouteMetric 1 -PolicyStore ActiveStore -Confirm:$false | Out-Null",
		del: "Remove-NetRoute -DestinationPrefix '0.0.0.0/0' -InterfaceIndex " + tunIdx + " -Confirm:$false -ErrorAction SilentlyContinue",
	})

	var applied []op
	for _, o := range ops {
		if _, err := psRun(o.add); err != nil {
			// 已存在(=既有直连, 正是 keep-local 想要的)视为成功但无需清理, 不记入 applied。
			log.Printf("orbit: global: route add %v", err)
		} else {
			applied = append(applied, o)
		}
	}
	return func() {
		for i := len(applied) - 1; i >= 0; i-- {
			_, _ = psRun(applied[i].del)
		}
	}, nil
}

// applySmartRoutes 智能模式: 把每条规则的目标前缀都接管进 TUN。
// 命中 0.0.0.0/0(=接管默认路由)时语义与 global 一致, 直接复用全局接管。
func applySmartRoutes(c *Client) (func(), error) {
	prefixes := smartPrefixes(c)
	if len(prefixes) == 0 {
		log.Printf("orbit: smart: no usable rule target, no route installed")
		return func() {}, nil
	}
	for _, p := range prefixes {
		if coversDefault(p) {
			return applyGlobalRoutes(c)
		}
	}
	tunIP := c.localIP.String()
	tunIdx, err := findTunIndex(c)
	if err != nil {
		return nil, fmt.Errorf("smart routes: find tun iface: %w", err)
	}
	type op struct{ add, del string }
	var ops []op
	for _, p := range prefixes {
		prefix := p.String()
		ops = append(ops, op{
			add: "New-NetRoute -DestinationPrefix '" + prefix + "' -NextHop '" + tunIP + "' -InterfaceIndex " + tunIdx + " -RouteMetric 1 -PolicyStore ActiveStore -Confirm:$false | Out-Null",
			del: "Remove-NetRoute -DestinationPrefix '" + prefix + "' -InterfaceIndex " + tunIdx + " -Confirm:$false -ErrorAction SilentlyContinue",
		})
	}
	var applied []op
	for _, o := range ops {
		if _, err := psRun(o.add); err != nil {
			log.Printf("orbit: smart: route add %v", err)
		} else {
			applied = append(applied, o)
		}
	}
	return func() {
		for i := len(applied) - 1; i >= 0; i-- {
			_, _ = psRun(applied[i].del)
		}
	}, nil
}
