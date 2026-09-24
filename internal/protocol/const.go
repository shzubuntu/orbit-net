// Package protocol 数据面/控制面帧与消息定义。
// 与 ssvpn 同源的简单扩展设计: [type:1][len:2][payload], 控制帧 json。
// 扩展预留: 设备 token 鉴权(Device)、Session/Quota 控制消息、多区域租户 id。
package protocol

// 帧类型(1 byte)
const (
	FrameData byte = 0x10 // 数据面: payload = 原始 IP 包
	FrameCtrl byte = 0x20 // 控制面: payload = JSON(CtrlMsg)
	FramePing byte = 0x30 // 心跳请求
	FramePong byte = 0x31 // 心跳响应
)

// 控制消息类型
const (
	MsgHello     = "hello"
	MsgWelcome   = "welcome"
	MsgPeerJoin  = "peer_join"
	MsgPeerLeave = "peer_leave"
	MsgReject    = "reject"
	MsgSession   = "session" // 服务器下发的会话摘要(账户/设备/档位/配额)
	MsgQuota     = "quota"   // 服务器下发的配额变更/超限
)

// 认证模式
const (
	AuthPassword = "password" // 骨架期保留; M1 后对接注册流程
	AuthDevice   = "device"   // 设备 token: 带 account+device 标识
)

// MaxPayload 单帧 payload 上限(防恶意大帧, 16MB 够 IP 包 + 封装)
const MaxPayload = 16 * 1024 * 1024

// Mode 客户端模式预设(见 docs/DESIGN.md)。
// 底层是同一套引擎的配置组合, 不做三套内核。
type Mode string

const (
	ModeSimple Mode = "simple" // 仅虚拟网卡, 与同网络设备互通
	ModeSmart  Mode = "smart"  // 按域名/IP 走出口 + 可开放本机为出口
	ModeGlobal Mode = "global" // 全流量经出口
)
