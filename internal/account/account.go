// Package account 控制面数据模型: 账户、设备、IP 租约、用量。
// 独立于数据面实体, 未来多区域数据面实例共享同一批账户(见 docs/DESIGN.md §Extensions)。
package account

import "time"

// Account 用户账户。公网用户走邀请码注册(int无密码 bcrypt, M1 落地)。
type Account struct {
	ID        string    `json:"id"` // 全局唯一; 多区域后保留区域前缀
	Name      string    `json:"name"`
	Tier      string    `json:"tier"` // free | pro | ... 预留计费档位
	CreatedAt time.Time `json:"created_at"`
}

// Device 已绑定设备(每账户多端)。
type Device struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	TokenHash string `json:"token_hash"` // bcrypt; 骨架期存明文, TODO(M1) 换 x/crypto
	// 出口授权开关: 本机是否开放为出口。谁可以用我 -> EgressACL。
	EgressEnabled bool      `json:"egress_enabled"`
	EgressACL     []string  `json:"egress_acl,omitempty"` // 允许消费我的账号(空=仅本人)
	CreatedAt     time.Time `json:"created_at"`
	LastSeen      time.Time `json:"last_seen"`
}
