package server

import (
	"strconv"
	"sync"
	"time"

	"orbit/internal/account"
)

// errQuotaBlocked 配额硬限拦截: 数据面静默跳过(不踢线), 客户端已收过超限通知。
type errQuotaBlocked struct{ reason string }

func (e *errQuotaBlocked) Error() string { return "quota blocked: " + e.reason }

// isQuotaBlocked 判断是否配额拦截类错误。
func isQuotaBlocked(err error) bool {
	_, ok := err.(*errQuotaBlocked)
	return ok
}

// quotaTrk 免费内测配额硬限(QuotaEnforcer, DESIGN §2.6/§5 "超限即停")。
// 以服务器中枢逐包计量为事实来源: 内存累计 O(1) 判定, 与 UsageStore 同源递增;
// 自然日边界自动清零。限制值 0 = 不限。
//
// 两条独立泳道:
//   - 账户日流量(bytes_per_day): 该账户所有转发字节(双向), 含 mesh 与出口;
//   - 消费者出口日流量(egress_bytes_per_day): 消费者设备经出口节点的双向字节
//     (与 usage.EgressLaneTotals 口径一致, 消费者视角)。
type quotaTrk struct {
	// defaults 未命中具体档位时的兜底配额。
	defaults QuotaSpec
	// tiers 计费档位表: tier名 -> 当日配额规格; 与 acct 一起启用分档(M3)。
	tiers map[string]QuotaSpec
	acct  *account.Store
	mu    sync.Mutex
	day   int64 // 当前记账自然日 time.Now().Unix()/86400
	// 账户当日已转发字节(全隧道, 双向累计)
	acctBy map[string]int64
	// 消费者设备当日经出口节点使用的字节
	egressBy map[string]int64
}

// newQuotaTrk 构造并回填当日已用量。
// 回填轴: 逐条租约(覆盖所有历史账户/设备)读 usage 落盘当日汇总, 防同日重启后配额被轻易击穿。
func newQuotaTrk(defaults QuotaSpec, usage *account.UsageStore, leases *account.LeaseStore) *quotaTrk {
	q := &quotaTrk{
		defaults: defaults,
		day:      time.Now().Unix() / 86400,
		acctBy:   map[string]int64{},
		egressBy: map[string]int64{},
	}
	if usage == nil || leases == nil {
		return q
	}
	since := time.Now().Truncate(24 * time.Hour)
	for _, l := range leases.Array() {
		if _, ok := q.acctBy[l.OwnerAccount]; !ok {
			t := usage.Totals(l.OwnerAccount, since)
			q.acctBy[l.OwnerAccount] = t.RxBytes + t.TxBytes
		}
		if _, ok := q.egressBy[l.OwnerDevice]; !ok {
			e := usage.EgressLaneTotals(l.OwnerDevice, since)
			q.egressBy[l.OwnerDevice] = e.RxBytes + e.TxBytes
		}
	}
	return q
}

// enableTiers 启用分档配额(仅 Server 启动时装一次)。
// 之后 specFor 对每个账户按 tier 解析; 空表/未命中档位回落 defaults。
func (q *quotaTrk) enableTiers(tiers map[string]QuotaSpec, acct *account.Store) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tiers = tiers
	q.acct = acct
}

// specFor 账户档位配额解析(只读; tiers/acct 启动后不变, Account() 自带 Store 锁)。
func (q *quotaTrk) specFor(accountID string) QuotaSpec {
	if q.tiers == nil || q.acct == nil {
		return q.defaults
	}
	if a, err := q.acct.Account(accountID); err == nil {
		if sp, ok := q.tiers[a.Tier]; ok {
			return sp
		}
	}
	return q.defaults
}

// admit 判定本包是否会被硬限拒绝(只读不记账): 返回 errQuotaBlocked 表示拒发。
func (q *quotaTrk) admit(accountID, consumerDeviceID string, egressPath bool, n int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked()
	spec := q.specFor(accountID)
	if spec.BytesPerDay > 0 {
		if used, limit := q.acctBy[accountID], spec.BytesPerDay; used+n > limit {
			return &errQuotaBlocked{"account daily bytes " + strconv.FormatInt(used+n, 10) + "/" + strconv.FormatInt(limit, 10)}
		}
	}
	if egressPath && spec.EgressBytesPerDay > 0 {
		if used, limit := q.egressBy[consumerDeviceID], spec.EgressBytesPerDay; used+n > limit {
			return &errQuotaBlocked{"egress daily bytes " + strconv.FormatInt(used+n, 10) + "/" + strconv.FormatInt(limit, 10)}
		}
	}
	return nil
}

// inc 认账(包成功转发后调用, 与 usage.Add 同源)。egressPath 计出口泳道。
func (q *quotaTrk) inc(accountID, consumerDeviceID string, egressPath bool, n int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked()
	spec := q.specFor(accountID)
	if spec.BytesPerDay <= 0 && spec.EgressBytesPerDay <= 0 {
		return
	}
	q.acctBy[accountID] += n
	if egressPath {
		q.egressBy[consumerDeviceID] += n
	}
}

// rolloverLocked 自然日切换清零(caller 持锁)。
func (q *quotaTrk) rolloverLocked() {
	if day := time.Now().Unix() / 86400; day != q.day {
		q.day = day
		clear(q.acctBy)
		clear(q.egressBy)
	}
}
