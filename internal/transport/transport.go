// Package transport 数据面传输抽象: 星型中继(Relay)与未来 P2P 直连(Direct)。
// 服务器作中枢计数的前提就是所有流量都过 Relay; Direct(M3)接入后补客户端记账上报。
package transport

import "errors"

// Conn 一条双向隧道(中继 = 客户端<->server, 直连 = 客户端<->客户端)。
type Conn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// Transport 传输后端接口。预留实现:
//   - relayTransport: 经服务器中继(M1)
//   - directTransport: P2P(NAT 打洞, M3)
type Transport interface {
	Name() string
	Dial(addr string) (Conn, error)
}

// ErrNotImplemented 传输尚未落地。
var ErrNotImplemented = errors.New("transport not implemented")

// relayTransport 占位实现; M1 落地。
type relayTransport struct{}

// Relay 返回中继传输(骨架期返回占位)。
func Relay() Transport { return &relayTransport{} }

func (*relayTransport) Name() string { return "relay" }

func (*relayTransport) Dial(string) (Conn, error) {
	return nil, ErrNotImplemented
}
