// Package server orbitd 服务端: 账户/设备控制面 + 数据面中继 + 管理 API。
// 数据面按 "networkID(=账户ID)+虚拟IP" 双键路由,默认每账户一个独立 /24
// 虚拟网络,跨账户天然隔离;未来群组共享网络时切换 networkID 即可。
package server

import (
	"errors"
	"fmt"
	"os"

	"orbit/internal/account"
)

// Config orbitd 配置。
type Config struct {
	ListenAddr string    `yaml:"listen_addr"` // 数据面 TLS 监听
	TLS        TLSConfig `yaml:"tls"`
	// Networks: 默认子网(key "default")。单机 M1 每账户复用该网段(账户隔离)。
	// 预留多区域: 扩展为 "region:network" 两级,各数据面实例各自解析。
	Networks     map[string]string `yaml:"networks"`
	Resolver     string            `yaml:"resolver"` // account(默认) | group(预留)
	Admin        AdminConfig       `yaml:"admin"`
	Public       PublicConfig      `yaml:"public"`   // 公共 HTTPS 自助入口(可选)
	DataDir      string            `yaml:"data_dir"` // 账户/租约/用量/邀请码落盘
	LogFile      string            `yaml:"log_file"`
	LogMaxBytes  int               `yaml:"log_max_bytes"`
	LogKeep      int               `yaml:"log_keep"`
	DefaultQuota QuotaSpec         `yaml:"default_quota"` // 未命中档位时的兜底配额(0=不限)
	DefaultTier  string            `yaml:"default_tier"`  // 新账户默认档位(空="free")
	// Tiers 计费档位表(M3): tier名 -> 当日配额规格。注册/自助查询/数据面转发均按账户 tier 解析,
	// 未列出或未命中的档位回落 DefaultQuota。tier 名区分大小写。
	Tiers map[string]QuotaSpec `yaml:"tiers"`
}

// TLSConfig 数据面 TLS。
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// AdminConfig 管理 HTTP API(仅本机/内网可达)。
type AdminConfig struct {
	Addr  string `yaml:"addr"`
	Token string `yaml:"token"` // X-Admin-Token
}

// PublicConfig 公共 HTTPS 入口: 承载客户端注册/自助管理/CA 下发。
// 与数据面共用同一张 TLS 证书; 空 Addr = 不启用。
type PublicConfig struct {
	Addr   string `yaml:"addr"`    // 如 0.0.0.0:4431
	CAFile string `yaml:"ca_file"` // 客户端可下载的 CA 证书路径(经 /ca.pem 下发)
}

// QuotaSpec 配额规格。
type QuotaSpec struct {
	BytesPerDay       int64 `yaml:"bytes_per_day"`
	EgressBytesPerDay int64 `yaml:"egress_bytes_per_day"`
}

// QuotaFor 按账户档位解析当日配额规格: tiers[tier] 命中则用, 否则回落 DefaultQuota。
func (c *Config) QuotaFor(tier string) QuotaSpec {
	if c.Tiers != nil {
		if q, ok := c.Tiers[tier]; ok {
			return q
		}
	}
	return c.DefaultQuota
}

// NetworkCIDR 取默认子网 CIDR。
func (c *Config) NetworkCIDR() string {
	if cidr := c.Networks["default"]; cidr != "" {
		return cidr
	}
	return "10.0.0.0/24"
}

// New 构造服务端: 加载账户/租约/用量/邀请码,建 Hub 与 TLS 监听器。
func New(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("listen_addr required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("data_dir required")
	}
	if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
		return nil, errors.New("tls.cert_file and tls.key_file required")
	}
	dir := cfg.DataDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("data_dir: %w", err)
	}

	acct, err := account.Open(dir + "/accounts.json")
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	leases, err := account.OpenLeases(dir+"/leases.json", cfg.NetworkCIDR())
	if err != nil {
		return nil, fmt.Errorf("leases: %w", err)
	}
	usage, err := account.OpenUsage(dir + "/usage.json")
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	invites, err := account.OpenInvites(dir + "/invites.json")
	if err != nil {
		return nil, fmt.Errorf("invites: %w", err)
	}

	hub := NewHub(cfg.NetworkCIDR(), cfg.DefaultQuota, leases, usage)
	if cfg.DefaultTier == "" {
		cfg.DefaultTier = "free"
	}
	hub.SetTierQuota(cfg.Tiers, acct)
	ln, err := listenTLS(cfg.ListenAddr, cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.ListenAddr, err)
	}

	return &Server{
		cfg:     cfg,
		acct:    acct,
		leases:  leases,
		usage:   usage,
		invites: invites,
		hub:     hub,
		ln:      ln,
		lim:     newRateLimiter(),
	}, nil
}
