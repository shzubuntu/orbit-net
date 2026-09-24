//go:build linux

package client

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
)

// defRoute 当前生效(metric 最小)的默认路由。
type defRoute struct{ Gateway, Dev string }

func defaultRoute() (*defRoute, error) {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "default") {
			continue
		}
		f := strings.Fields(line)
		r := &defRoute{}
		for i, tok := range f {
			if tok == "via" && i+1 < len(f) {
				r.Gateway = f[i+1]
			}
			if tok == "dev" && i+1 < len(f) {
				r.Dev = f[i+1]
			}
		}
		if r.Gateway != "" && r.Dev != "" {
			return r, nil
		}
	}
	return nil, nil // 本机无默认路由
}

// routeOp 一条可增可删的路由命令(addArgs 全量, delArgs 为 ip route del 参数)。
type routeOp struct {
	addArgs []string
	delArgs []string
}

func (o routeOp) add() error {
	if out, err := exec.Command("ip", o.addArgs...).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "File exists") {
			return nil
		}
		return fmt.Errorf("ip %v: %s", o.addArgs, strings.TrimSpace(string(out)))
	}
	return nil
}

func (o routeOp) del() {
	_ = exec.Command("ip", o.delArgs...).Run()
}

// applyGlobalRoutes 全局模式: 接管默认路由到 TUN; 服务器/keep-local 保留直连。
// 只新增不删除原有默认路由(凭 metric 优先级胜出), 恢复即删新增。
func applyGlobalRoutes(c *Client) (func(), error) {
	var ops []routeOp
	dr, err := defaultRoute()
	if err != nil {
		return nil, fmt.Errorf("global routes: read default: %w", err)
	}
	tunName := c.tunDev.Name()

	if dr != nil && (len(c.cfg.KeepLocal) > 0 || len(serverHostIPs(c.cfg.ServerAddr)) > 0) {
		keep := append([]string{}, c.cfg.KeepLocal...)
		for _, ip := range serverHostIPs(c.cfg.ServerAddr) {
			keep = append(keep, ip.String())
		}
		for _, spec := range keep {
			var cidr string
			if _, _, e := net.ParseCIDR(spec); e == nil {
				cidr = spec
			} else if ip := net.ParseIP(strings.TrimSpace(spec)); ip != nil {
				cidr = ip.String() + "/32"
			} else {
				log.Printf("orbit: global: skip keep-local %q", spec)
				continue
			}
			a := []string{"route", "add", cidr, "via", dr.Gateway, "dev", dr.Dev, "metric", "1"}
			d := []string{"route", "del", cidr, "via", dr.Gateway, "dev", dr.Dev}
			ops = append(ops, routeOp{a, d})
		}
	}

	// 接管默认: 低 metric 生效, 原有默认路由保留不删
	a := []string{"route", "add", "default", "dev", tunName, "metric", "10"}
	d := []string{"route", "del", "default", "dev", tunName, "metric", "10"}
	ops = append(ops, routeOp{a, d})

	for _, o := range ops {
		if err := o.add(); err != nil {
			// 失败回滚已添加的路由
			for i := len(ops) - 1; i >= 0; i-- {
				ops[i].del()
			}
			return nil, fmt.Errorf("global routes: %w", err)
		}
	}
	return func() {
		for i := len(ops) - 1; i >= 0; i-- {
			ops[i].del()
		}
	}, nil
}

// applySmartRoutes 智能模式: 把规则目标前缀接管进 TUN(与 global 同款 low-metric 屏蔽,
// 原路由保留不删, 退出时仅删新增)。命中 0.0.0.0/0 时复用全局默认接管。
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
	tunName := c.tunDev.Name()
	var ops []routeOp
	for _, p := range prefixes {
		cidr := p.String()
		ops = append(ops, routeOp{
			addArgs: []string{"route", "add", cidr, "dev", tunName, "metric", "10"},
			delArgs: []string{"route", "del", cidr, "dev", tunName, "metric", "10"},
		})
	}
	for _, o := range ops {
		if err := o.add(); err != nil {
			for i := len(ops) - 1; i >= 0; i-- {
				ops[i].del()
			}
			return nil, fmt.Errorf("smart routes: %w", err)
		}
	}
	return func() {
		for i := len(ops) - 1; i >= 0; i-- {
			ops[i].del()
		}
	}, nil
}
