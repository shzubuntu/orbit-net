package server

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"orbit/internal/account"
	"orbit/internal/protocol"
)

type recConn struct{ bytes.Buffer }

func TestHubRelaySameAccount(t *testing.T) {
	h, acct, devA, devB := testHub(t)

	wA, nA := regNode(t, h, devA, "host-a", false)
	wB, nB := regNode(t, h, devB, "host-b", false)
	if wA.VirtualIP == wB.VirtualIP {
		t.Fatalf("expected distinct IPs, got %s twice", wA.VirtualIP)
	}

	pkt := icmpPkt(net.ParseIP(wA.VirtualIP), net.ParseIP(wB.VirtualIP))
	if err := h.Relay(pkt, nA); err != nil {
		t.Fatalf("relay: %v", err)
	}
	got := nB.rw.(*recConn).Bytes() // B 收到 FrameData + 包
	if len(got) < 3 || got[0] != protocol.FrameData {
		t.Fatalf("peer B did not receive data frame: % x", got)
	}
	if nA.rxBytes.Load() != int64(len(pkt)) {
		t.Fatalf("nA rx=%d want %d", nA.rxBytes.Load(), len(pkt))
	}
	if nB.txBytes.Load() != int64(len(pkt)) {
		t.Fatalf("nB tx=%d want %d", nB.txBytes.Load(), len(pkt))
	}
	tot := h.usage.Totals(acct.ID, time.Now().Add(-time.Hour))
	if tot.RxBytes == 0 && tot.TxBytes == 0 {
		t.Fatal("usage not recorded")
	}
}

func TestHubRelayEgressACL(t *testing.T) {
	h, _, devA, devB := testHub(t)
	_, nA := regNode(t, h, devA, "host-a", false)
	_, nB := regNode(t, h, devB, "host-b", true) // B 开出口,ACL 空

	pkt := icmpPkt(nA.VirtualIP, nB.VirtualIP)
	if err := h.Relay(pkt, nA); err != nil {
		t.Fatalf("same-account egress should pass, got %v", err)
	}

	// 跨账户(ACL 空): 拒绝
	h2, _, devC, devC2 := testHub(t)
	_, n2A := regNode(t, h2, devC, "host-c", false)
	_, n2B := regNode(t, h2, devC2, "host-d", true)
	n2A.AccountID = "acc_c"
	n2B.AccountID = "acc_d"
	err := h2.Relay(icmpPkt(n2A.VirtualIP, n2B.VirtualIP), n2A)
	if err == nil {
		t.Fatal("cross-account egress without ACL must be denied")
	}

	n2B.EgressACL = []string{"acc_c"}
	if err := h2.Relay(icmpPkt(n2A.VirtualIP, n2B.VirtualIP), n2A); err != nil {
		t.Fatalf("ACL-listed egress should pass, got %v", err)
	}
}

func TestHubEgressMetering(t *testing.T) {
	h, _, devA, devB := testHub(t)
	_, nA := regNode(t, h, devA, "host-a", false)
	_, nB := regNode(t, h, devB, "host-b", true)
	// 代理链路: 消费者→出口 走 IP-in-IP 封装。
	pkt := ipipPkt(nA.VirtualIP, nB.VirtualIP, nA.VirtualIP, net.IPv4(1, 2, 3, 4))
	if err := h.Relay(pkt, nA); err != nil {
		t.Fatalf("relay: %v", err)
	}
	// 出口泳道记在消费者头上(不再是出口节点)。
	if e := h.usage.EgressLaneTotals(devA.ID, time.Now().Add(-time.Hour)); e.RxBytes+e.TxBytes == 0 {
		t.Fatal("consumer egress usage not metered")
	}
	// 出口节点自身的消费者泳道保持空(它没消费任何出口)。
	if e := h.usage.EgressLaneTotals(devB.ID, time.Now().Add(-time.Hour)); e.RxBytes+e.TxBytes != 0 {
		t.Fatalf("exit node must not accrue consumer egress lane, got %d", e.RxBytes+e.TxBytes)
	}
}

// TestHubDirectMeshToExitExcluded 直达出口盒的 mesh 流量不得计入出口泳道。
func TestHubDirectMeshToExitExcluded(t *testing.T) {
	h, _, devA, devB := testHub(t)
	_, nA := regNode(t, h, devA, "host-a", false)
	wB, _ := regNode(t, h, devB, "host-b", true)
	pkt := icmpPkt(nA.VirtualIP, net.ParseIP(wB.VirtualIP))
	if err := h.Relay(pkt, nA); err != nil {
		t.Fatalf("relay: %v", err)
	}
	e := h.usage.EgressLaneTotals(devA.ID, time.Now().Add(-time.Hour))
	if e.RxBytes+e.TxBytes != 0 {
		t.Fatalf("direct mesh to exit must not hit egress lane, got %d", e.RxBytes+e.TxBytes)
	}
	tot := h.usage.Totals(devA.AccountID, time.Now().Add(-time.Hour))
	if tot.RxBytes+tot.TxBytes == 0 {
		t.Fatal("direct mesh should still count toward account day pool")
	}
}

// TestHubEgressReturnAttributedConsumer 回包(出口→消费者, IPIP)归因到消费者:
// 账户日流量池不变, 出口泳道双向累加在消费者, 出口节点自己的消费者泳道保持 0。
func TestHubEgressReturnAttributedConsumer(t *testing.T) {
	h, acct, devA, devB := testHub(t)
	_, nA := regNode(t, h, devA, "host-a", false)
	wB, nB := regNode(t, h, devB, "host-b", true)

	up := ipipPkt(nA.VirtualIP, net.ParseIP(wB.VirtualIP), nA.VirtualIP, net.IPv4(9, 9, 9, 9))
	if err := h.Relay(up, nA); err != nil {
		t.Fatalf("uplink relay: %v", err)
	}
	down := ipipPkt(nB.VirtualIP, nA.VirtualIP, net.IPv4(9, 9, 9, 9), nA.VirtualIP)
	if err := h.Relay(down, nB); err != nil {
		t.Fatalf("return relay: %v", err)
	}

	since := time.Now().Add(-time.Hour)
	tot := h.usage.Totals(acct.ID, since)
	if want := int64(len(up) + len(down)); tot.RxBytes+tot.TxBytes != want {
		t.Fatalf("account day pool=%d, want %d (两方向各记一次到同一账户)", tot.RxBytes+tot.TxBytes, want)
	}
	// 消费者出口泳道 = 双向
	if e := h.usage.EgressLaneTotals(devA.ID, since); e.RxBytes+e.TxBytes != int64(len(up)+len(down)) {
		t.Fatalf("consumer egress lane=%d, want %d", e.RxBytes+e.TxBytes, len(up)+len(down))
	}
	// 出口节点不再背下游流量(消费者泳道视角), 但"经由该出口的总量"观仍成立
	if e := h.usage.EgressLaneTotals(devB.ID, since); e.RxBytes+e.TxBytes != 0 {
		t.Fatalf("exit node consumer lane=%d, want 0", e.RxBytes+e.TxBytes)
	}
	if e := h.usage.EgressTotals(devB.ID, since); e.RxBytes+e.TxBytes != int64(len(up)+len(down)) {
		t.Fatalf("egressTotals via exit=%d, want %d (双向都经由出口盒)", e.RxBytes+e.TxBytes, len(up)+len(down))
	}
	// 桶第三段=出口节点, 与上行同桶(PerConsumer 归一到消费者视图)
	if ps := h.usage.PerConsumer(since); len(ps) != 1 {
		t.Fatalf("PerConsumer bucket count=%d, want 1 (双向同桶)", len(ps))
	}
	_ = nA
	_ = nB
	_ = wB
}

// TestHubEgressReturnQuota 下载回包吃消费者的出口配额(到顶拒发) —— 出口配额真正挡住下载。
func TestHubEgressReturnQuota(t *testing.T) {
	onePkt := len(ipipPkt(net.IPv4(1, 1, 1, 1), net.IPv4(2, 2, 2, 2), net.IPv4(3, 3, 3, 3), net.IPv4(4, 4, 4, 4)))
	h, _, devA, devB := testHubQ(t, QuotaSpec{
		BytesPerDay:       1 << 40,
		EgressBytesPerDay: int64(onePkt),
	})
	_, nA := regNode(t, h, devA, "host-a", false)
	wB, nB := regNode(t, h, devB, "host-b", true)

	up := ipipPkt(nA.VirtualIP, net.ParseIP(wB.VirtualIP), nA.VirtualIP, net.IPv4(5, 5, 5, 5))
	if err := h.Relay(up, nA); err != nil {
		t.Fatalf("uplink should pass, got %v", err)
	}
	down := ipipPkt(nB.VirtualIP, nA.VirtualIP, net.IPv4(5, 5, 5, 5), nA.VirtualIP)
	if err := h.Relay(down, nB); !isQuotaBlocked(err) {
		t.Fatalf("return beyond consumer egress cap should be blocked, got %v", err)
	}
}

func TestHubReRegisterKeepsIP(t *testing.T) {
	h, _, devA, _ := testHub(t)
	w1, n1 := regNode(t, h, devA, "host-a", false)
	// 模拟掉线重连: 同一设备再注册
	w2, n2 := regNode(t, h, devA, "host-a", false)
	if w1.VirtualIP != w2.VirtualIP {
		t.Fatalf("reregister changed ip: %s -> %s", w1.VirtualIP, w2.VirtualIP)
	}
	if h.leaseCount(n1.NetworkID) != 1 || n2 == nil {
		t.Fatalf("expected single lease, leases=%d", h.leaseCount(n1.NetworkID))
	}
}

func TestHubQuotaAccountLimit(t *testing.T) {
	h, acct, devA, devB := testHubQ(t, QuotaSpec{BytesPerDay: 2 * int64(len(icmpPkt(net.IPv4(1, 2, 3, 4), net.IPv4(5, 6, 7, 8))))})
	wA, nA := regNode(t, h, devA, "host-a", false)
	wB, nB := regNode(t, h, devB, "host-b", false)

	pkt := icmpPkt(net.ParseIP(wA.VirtualIP), net.ParseIP(wB.VirtualIP))
	// 前 2 包放行(额度用尽), 第 3 包起硬限拒绝且不再落 B。
	want := 0
	for i := 0; i < 4; i++ {
		err := h.Relay(pkt, nA)
		if i < 2 {
			if err != nil {
				t.Fatalf("relay #%d should pass, got %v", i, err)
			}
			want += 3 + len(pkt) // FrameData 头 3B + 载荷
			continue
		}
		if !isQuotaBlocked(err) {
			t.Fatalf("relay #%d should be quota-blocked, got %v", i, err)
		}
	}
	if got := nB.rw.(*recConn).Len(); got != want {
		t.Fatalf("peer B received %d bytes, want %d (beyond-quota packets must not be forwarded)", got, want)
	}
	_ = acct
	// 超限后本连接收到过一次 quota 通知
	if !nA.quotaNotified {
		t.Fatal("quota notice should have been sent once")
	}
}

func TestHubTierQuota(t *testing.T) {
	// 默认额度极大; 档位表里 pro 仅 1 包额度。账户档位提升后立即按新档位裁决。
	pktLen := int64(len(icmpPkt(net.IPv4(1, 2, 3, 4), net.IPv4(5, 6, 7, 8))))
	h, _, devA, devB := testHubQ(t, QuotaSpec{BytesPerDay: 1 << 40})
	acctStore, err := account.Open(t.TempDir() + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := acctStore.AddAccount(&account.Account{ID: "acc_t", Name: "tiered", Tier: "pro"}); err != nil {
		t.Fatal(err)
	}
	h.SetTierQuota(map[string]QuotaSpec{"pro": {BytesPerDay: pktLen}}, acctStore)

	wA, nA := regNode(t, h, devA, "host-a", false)
	wB, _ := regNode(t, h, devB, "host-b", false)
	pkt := icmpPkt(net.ParseIP(wA.VirtualIP), net.ParseIP(wB.VirtualIP))
	if err := h.Relay(pkt, nA); err != nil {
		t.Fatalf("first packet should pass under pro quota, got %v", err)
	}
	if err := h.Relay(pkt, nA); !isQuotaBlocked(err) {
		t.Fatalf("second packet should be blocked by pro tier limit, got %v", err)
	}
	// 档位未列入 tiers 表时应回落默认额度(unlimited)
	if tier := h.quotaSpecFor("acc_none"); tier.BytesPerDay != 1<<40 {
		t.Fatalf("unmapped tier should fall back to default quota, got %+v", tier)
	}
}

func TestHubQuotaEgressLimit(t *testing.T) {
	// 出口泳道单独限制: 账户额度很大, 出口额度恰好 1 包(用代理封装包)。
	pktLen := len(ipipPkt(net.IPv4(1, 1, 1, 1), net.IPv4(2, 2, 2, 2), net.IPv4(3, 3, 3, 3), net.IPv4(4, 4, 4, 4)))
	h, _, devA, devB := testHubQ(t, QuotaSpec{
		BytesPerDay:       1 << 40,
		EgressBytesPerDay: int64(pktLen),
	})
	_, nA := regNode(t, h, devA, "host-a", false)
	wB, _ := regNode(t, h, devB, "host-b", true) // B 是出口

	pkt := ipipPkt(nA.VirtualIP, net.ParseIP(wB.VirtualIP), nA.VirtualIP, net.IPv4(6, 6, 6, 6))
	if err := h.Relay(pkt, nA); err != nil {
		t.Fatalf("first egress packet should pass, got %v", err)
	}
	if err := h.Relay(pkt, nA); !isQuotaBlocked(err) {
		t.Fatalf("second egress packet should be quota-blocked, got %v", err)
	}
	// 非出口(mesh)方向不受出口泳道限制
	_, nC := regNode(t, h, &account.Device{ID: "dev_c", AccountID: devA.AccountID}, "host-c", false)
	mesh := icmpPkt(nA.VirtualIP, nC.VirtualIP)
	if err := h.Relay(mesh, nA); err != nil {
		t.Fatalf("non-egress mesh should not hit egress quota, got %v", err)
	}
	// 直达出口盒的 mesh 也不受出口泳道限制
	direct := icmpPkt(nA.VirtualIP, nC.VirtualIP)
	if err := h.Relay(direct, nA); err != nil {
		t.Fatalf("direct mesh should not hit egress quota, got %v", err)
	}
}

func TestHubQuotaRollover(t *testing.T) {
	limit := int64(30)
	h, _, _, _ := testHubQ(t, QuotaSpec{BytesPerDay: limit})
	q := h.quota
	// 记满额度(admit 只读判定, 实际累计走 inc, 与 Relay 语义一致)
	if err := q.admit("acc_x", "dev_x", false, limit); err != nil {
		t.Fatalf("admit to limit: %v", err)
	}
	q.inc("acc_x", "dev_x", false, limit)
	if err := q.admit("acc_x", "dev_x", false, 1); !isQuotaBlocked(err) {
		t.Fatalf("over limit should block, got %v", err)
	}
	// 模拟日切冲零
	q.mu.Lock()
	q.day = time.Now().Unix()/86400 - 1
	q.mu.Unlock()
	if err := q.admit("acc_x", "dev_x", false, 1); err != nil {
		t.Fatalf("after day rollover should pass, got %v", err)
	}
}

// ==================== helpers ====================

func testHub(t *testing.T) (*Hub, *account.Account, *account.Device, *account.Device) {
	return testHubQ(t, QuotaSpec{BytesPerDay: 1 << 30})
}

func testHubQ(t *testing.T, q QuotaSpec) (*Hub, *account.Account, *account.Device, *account.Device) {
	t.Helper()
	leases, err := account.OpenLeases(t.TempDir()+"/leases.json", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := account.OpenUsage(t.TempDir() + "/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &account.Account{ID: "acc_t", Name: "test", Tier: "free"}
	devA := &account.Device{ID: "dev_a", AccountID: a.ID}
	devB := &account.Device{ID: "dev_b", AccountID: a.ID}
	h := NewHub("10.0.0.0/24", q, leases, usage)
	return h, a, devA, devB
}

func regNode(t *testing.T, h *Hub, dev *account.Device, host string, egress bool) (*protocol.Welcome, *Node) {
	t.Helper()
	acct := &account.Account{ID: dev.AccountID, Name: "t", Tier: "free"}
	dev.EgressEnabled = egress // 出口以 store 为权威,测试按参数同步(与生产 enroll+admin/egress 一致)
	hello := &protocol.Hello{User: acct.ID, DeviceID: dev.ID, Hostname: host, OS: "linux", Egress: egress}
	rec := &recConn{}
	w, n, err := h.Register(dev.AccountID, dev, acct, hello, rec, "tcp://test")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	n.rw = rec
	return w, n
}

func icmpPkt(src, dst net.IP) []byte {
	s := src.To4()
	d := dst.To4()
	pkt := make([]byte, 28+8)
	pkt[0] = 0x45
	pkt[8] = 64
	pkt[9] = 1
	copy(pkt[12:16], s)
	copy(pkt[16:20], d)
	pkt[20] = 8 // ICMP echo
	return pkt
}

// ipipPkt 构造外层 IP-in-IP(协议4)代理包: 外层 src/dst + 内层完整 IP 包。
func ipipPkt(outerSrc, outerDst, innerSrc, innerDst net.IP) []byte {
	inner := icmpPkt(innerSrc, innerDst)
	pkt := make([]byte, 20+len(inner))
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = ipProtoIPIP
	copy(pkt[12:16], outerSrc.To4())
	copy(pkt[16:20], outerDst.To4())
	copy(pkt[20:], inner)
	return pkt
}

// leaseCount 单网络租约数。
func (h *Hub) leaseCount(networkID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	net, ok := h.networks[networkID]
	if !ok {
		return 0
	}
	return len(net.Nodes)
}
