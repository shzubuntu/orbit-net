package account

import (
	"path/filepath"
	"testing"
	"time"
)

// TestUsageRoundtrip 用量落盘往返(Bucket 结构体键序列化回归)。
func TestUsageRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.json")
	u, err := OpenUsage(p)
	if err != nil {
		t.Fatal(err)
	}
	u.Add("acc_x", "dev_x", "", 100, 200, time.Now())
	u.Add("acc_y", "dev_y", "dev_eg", 5, 6, time.Now())
	if err := u.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	u2, err := OpenUsage(p)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	if got := u2.Totals("acc_x", since); got.RxBytes != 100 || got.TxBytes != 200 {
		t.Fatalf("acc_x totals after reopen: %+v", got)
	}
	if got := u2.Totals("acc_y", since); got.RxBytes != 5 || got.TxBytes != 6 {
		t.Fatalf("acc_y totals after reopen: %+v", got)
	}
	if got := u2.EgressTotals("dev_eg", since); got.RxBytes != 5 || got.TxBytes != 6 {
		t.Fatalf("egress totals after reopen: %+v", got)
	}
	if got := u2.EgressLaneTotals("dev_y", since); got.RxBytes != 5 || got.TxBytes != 6 {
		t.Fatalf("egress lane (consumer view) after reopen: %+v", got)
	}
}

// TestUsageEgressLaneConsumerView 消费者泳道只认"该设备经出口"的桶, 直达桶不入列。
func TestUsageEgressLaneConsumerView(t *testing.T) {
	p := filepath.Join(t.TempDir(), "usage.json")
	u, err := OpenUsage(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	u.Add("acc", "dev_c", "", 0, 100, now)     // 直达 mesh, 不入泳道
	u.Add("acc", "dev_c", "dev_e", 0, 50, now) // 上行经出口
	u.Add("acc", "dev_c", "dev_e", 0, 70, now) // 回包经出口(同桶合并)
	u.Add("acc", "dev_e", "", 0, 200, now)     // 出口盒自身流量, 与消费者无关
	since := now.Add(-time.Hour)

	if got := u.EgressLaneTotals("dev_c", since); got.RxBytes != 0 || got.TxBytes != 120 {
		t.Fatalf("consumer lane dev_c: %+v, want tx=120", got)
	}
	if got := u.EgressLaneTotals("dev_e", since); got.RxBytes != 0 || got.TxBytes != 0 {
		t.Fatalf("consumer lane dev_e: %+v, want 0", got)
	}
	if got := u.EgressTotals("dev_e", since); got.TxBytes != 120 {
		t.Fatalf("via-exit totals dev_e: %+v, want tx=120", got)
	}
}
