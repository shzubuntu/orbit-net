package client

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"

	"orbit/internal/protocol"
)

// Config orbit-cli 配置。mode 决定最终展开的引擎开关组合(单一内核)。
type Config struct {
	ServerAddr  string `yaml:"server_addr"` // 数据面 TLS 地址 host:port
	CACertPath  string `yaml:"ca_cert_path"`
	Account     string `yaml:"account"`
	DeviceID    string `yaml:"device_id"`
	DeviceToken string `yaml:"device_token"` // 设备凭证(注册/加设备时的一次性 token)
	Hostname    string `yaml:"hostname"`
	TunName     string `yaml:"tun_name"` // 空=系统自动命名
	LogFile     string `yaml:"log_file"`
	LogMaxBytes int    `yaml:"log_max_bytes"`
	LogKeep     int    `yaml:"log_keep"`

	Mode   protocol.Mode `yaml:"mode"`   // simple|smart|global
	Egress bool          `yaml:"egress"` // 本机开放为出口

	ExitDevice string `yaml:"exit_device,omitempty"` // global 模式的默认出口(设备ID/主机名/IP), 空=自动取在线首个 egress 节点

	// SelfAPIBase 自助 API base(如 https://host:4431/api/v1), 供 orbit-cli devices 使用;
	// register 时写入, 空则回落默认服务器。
	SelfAPIBase string `yaml:"self_api_base,omitempty"`

	// simple: 无需规则; smart: 规则列表; global: 0.0.0.0/0 已隐含
	Rules     []RuleSpec `yaml:"rules"`
	KeepLocal []string   `yaml:"keep_local"` // 全局模式下排除的网段(服务器地址/虚拟网段等)
}

// RuleSpec 智能模式规则: 目标(Target) 走某个出口设备(ExitDevice)。
type RuleSpec struct {
	Target     string `yaml:"target"`      // 域名(含 *.通配)/IP/CIDR
	ExitDevice string `yaml:"exit_device"` // 出口设备名/ID, 空=服务器中继
}

// Load 读取并校验配置。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate 基础校验。
func (c *Config) Validate() error {
	if c.ServerAddr == "" {
		return errors.New("server_addr required")
	}
	switch c.Mode {
	case protocol.ModeSimple, protocol.ModeSmart, protocol.ModeGlobal:
	default:
		c.Mode = protocol.ModeSimple
	}
	if c.Mode == protocol.ModeGlobal && c.Egress {
		return errors.New("mode global and egress cannot combine (全流量进隧道, 出口节点会回环)")
	}
	return nil
}

// Save 原子写回配置(临时文件 + 改名, 避免半写): 供 orbit-cli set / orbit-gui 共用。
// 写入保持结构字段顺序, 无 BOM, 0600 仅属主可读(内含设备令牌)。
func (c *Config) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
