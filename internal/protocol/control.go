package protocol

import "encoding/json"

// CtrlMsg 控制面消息信封: 按 Type 挂具体载荷。
type CtrlMsg struct {
	Type      string     `json:"type"`
	Hello     *Hello     `json:"hello,omitempty"`
	Welcome   *Welcome   `json:"welcome,omitempty"`
	PeerJoin  *PeerInfo  `json:"peer_join,omitempty"`
	PeerLeave *PeerLeave `json:"peer_leave,omitempty"`
	Reject    *Reject    `json:"reject,omitempty"`
	Session   *Session   `json:"session,omitempty"`
	Quota     *Quota     `json:"quota,omitempty"`
}

// Hello 客户端握手(设备鉴权为主, 账号密码为骨架期兼容)。
type Hello struct {
	User     string `json:"user"`
	DeviceID string `json:"device_id"`
	Auth     Auth   `json:"auth"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Egress   bool   `json:"egress,omitempty"` // 本机开放为出口
	Mode     Mode   `json:"mode,omitempty"`
}

// Auth 认证载荷。
type Auth struct {
	Mode     string `json:"mode"`
	Password string `json:"password,omitempty"` // AuthPassword
	Token    string `json:"token,omitempty"`    // AuthDevice
}

// Welcome 握手成功(下发本机虚拟 IP 与同网在线设备)。
type Welcome struct {
	VirtualIP string     `json:"virtual_ip"`
	CIDR      string     `json:"cidr"`
	NetworkID string     `json:"network_id"`
	MTU       int        `json:"mtu"`
	Peers     []PeerInfo `json:"peers"`
	Session   *Session   `json:"session,omitempty"`
}

// PeerInfo 在线设备信息(server 广播 peer_join/初始 welcome)。
// Egress 字段供客户端展示"哪些节点可用作出口"。
type PeerInfo struct {
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Egress   bool   `json:"egress,omitempty"`
}

type PeerLeave struct {
	IP string `json:"ip"`
}

type Reject struct {
	Reason string `json:"reason"`
}

// Session 服务器下发的会话摘要(控制面快照)。
type Session struct {
	AccountID string `json:"account_id"`
	DeviceID  string `json:"device_id"`
	Tier      string `json:"tier"` // free | pro | ... (预留计费档位)
	Quota     *Quota `json:"quota,omitempty"`
}

// Quota 配额; 0 表示不限。Used(服务器中枢计数)。
type Quota struct {
	Period           string `json:"period"` // e.g. day
	UsedBytes        int64  `json:"used_bytes"`
	LimitBytes       int64  `json:"limit_bytes,omitempty"`
	EgressUsedBytes  int64  `json:"egress_used_bytes,omitempty"`
	EgressLimitBytes int64  `json:"egress_limit_bytes,omitempty"`
}

// EncodeCtrl 序列化控制消息。
func EncodeCtrl(m *CtrlMsg) ([]byte, error) {
	return json.Marshal(m)
}

// DecodeCtrl 解析控制消息。
func DecodeCtrl(b []byte) (*CtrlMsg, error) {
	var m CtrlMsg
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
