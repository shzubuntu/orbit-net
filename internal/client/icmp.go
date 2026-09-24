package client

import (
	"encoding/binary"
)

// 出口节点头部回显: 隧道里的 ICMP echo request 由本机直接生成 reply 回包,
// 不进 gVisor netstack(它只做 TCP/UDP)。支持 ping 验证与常见连通性探测,
// 其余 ICMP(差错包/time exceeded 等)本版不转发, 影响面为探测类用途。

// icmpEchoReply 若 packet 是发给任意目标的 ICMP echo request, 返回对应的 echo reply
// (交换 src/dst、type 改 0、重算校验和); 否则返回 nil。
func icmpEchoReply(packet []byte) []byte {
	// IPv4 头 20B + ICMP 头 8B 起步; 仅处理 IPv4 + 协议号 1
	if len(packet) < 28 || packet[0]>>4 != 4 || packet[9] != 1 {
		return nil
	}
	if packet[20] != 8 { // type 8 = echo request
		return nil
	}
	out := make([]byte, len(packet))
	copy(out, packet)
	copy(out[16:20], packet[12:16]) // dst <- src
	copy(out[12:16], packet[16:20]) // src <- dst
	out[20] = 0                     // echo reply
	out[21] = 0                     // code 0
	out[22], out[23] = 0, 0         // 校验和清零重算
	sum := icmpChecksum(out[20:])
	out[22] = byte(sum >> 8)
	out[23] = byte(sum)
	return out
}

// icmpChecksum ICMP 校验和(末字节补零, 同 IPv4 校验和算法)。
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
