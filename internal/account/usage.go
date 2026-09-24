package account

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bucket 用量明细键: 谁用了谁的出口, 按小时桶聚合。
// 计费与免费内测配额的唯一事实来源(服务器中枢计数)。
type Bucket struct {
	AccountID      string `json:"account_id"` // 消费者
	DeviceID       string `json:"device_id"`
	EgressDeviceID string `json:"egress_device_id,omitempty"` // 空=未走出口(内部)
	Hour           int64  `json:"hour"`                       // Unix 小时桶
}

// Counters 一个键的流量计数。单计口径: 每个成功转发的包在发送方桶记 tx=包长(见 hub.Relay),
// rx 当前恒为 0(保留维度)。避免旧版把 rx/tx 双写同一包导致的重复计数。
type Counters struct {
	RxBytes int64 `json:"rx_bytes"`
	TxBytes int64 `json:"tx_bytes"`
}

// UsageStore 用量聚合(内存 + 定期落盘 JSON)。M1 规模够用。
type UsageStore struct {
	mu      sync.RWMutex
	path    string
	buckets map[Bucket]*Counters
}

// OpenUsage 打开用量库。
func OpenUsage(path string) (*UsageStore, error) {
	u := &UsageStore{path: path, buckets: map[Bucket]*Counters{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		// 落盘格式: 字符串键(account|device|egress|hour) -> Counters。
		// 注: map[Bucket] 结构体键会被新 json 后端拒绝("object member name must be a string"),
		// 故持久化一律经 keyedBuckets 转 string 键。
		var rawMap map[string]*Counters
		if err := json.Unmarshal(raw, &rawMap); err != nil {
			return nil, err
		}
		for k, c := range rawMap {
			// 旧版账单迁移: 此前 hub.Relay 把每包 rx 与 tx 各记一次(双计, 与配额实时口径不符)。
			// 历史行 rx==tx==包长, 折叠为 tx=包长 → 单计; 仅对这类双计行生效, 不影响通用 rx/tx 行。
			if c.RxBytes == c.TxBytes {
				c.RxBytes = 0
			}
			parts := strings.SplitN(k, "|", 4)
			if len(parts) != 4 {
				continue
			}
			hour, _ := strconv.ParseInt(parts[3], 10, 64)
			u.buckets[Bucket{AccountID: parts[0], DeviceID: parts[1], EgressDeviceID: parts[2], Hour: hour}] = c
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return u, nil
}

// Add 累计流量。调用方约定单计: 每成功转发一个包记一次到发送方设备桶,
// 传 rx=0, tx=包长; egressDeviceID 非空表示该包发往出口节点(出口泳道)。
func (u *UsageStore) Add(accountID, deviceID, egressDeviceID string, rx, tx int64, at time.Time) {
	b := Bucket{
		AccountID:      accountID,
		DeviceID:       deviceID,
		EgressDeviceID: egressDeviceID,
		Hour:           at.Truncate(time.Hour).Unix(),
	}
	u.mu.Lock()
	c := u.buckets[b]
	if c == nil {
		c = &Counters{}
		u.buckets[b] = c
	}
	c.RxBytes += rx
	c.TxBytes += tx
	u.mu.Unlock()
}

// Totals 账户某时段内累计。
func (u *UsageStore) Totals(accountID string, since time.Time) Counters {
	u.mu.RLock()
	defer u.mu.RUnlock()
	var out Counters
	for b, c := range u.buckets {
		if b.AccountID == accountID && b.Hour >= since.Truncate(time.Hour).Unix() {
			out.RxBytes += c.RxBytes
			out.TxBytes += c.TxBytes
		}
	}
	return out
}

// EgressTotals 某设备作为出口被消费的累计(出口节点视角: 所有消费者经它的字节)。
func (u *UsageStore) EgressTotals(egressDeviceID string, since time.Time) Counters {
	u.mu.RLock()
	defer u.mu.RUnlock()
	var out Counters
	for b, c := range u.buckets {
		if b.EgressDeviceID == egressDeviceID && b.Hour >= since.Truncate(time.Hour).Unix() {
			out.RxBytes += c.RxBytes
			out.TxBytes += c.TxBytes
		}
	}
	return out
}

// EgressLaneTotals 消费者设备的出口泳道累计(消费者视角: 该设备经出口的双向字节,
// 含直达出口节点的 mesh 不在此列)。与配额 egressBy[consumer] 同口径, 供配额回填/会话/通知。
func (u *UsageStore) EgressLaneTotals(consumerDeviceID string, since time.Time) Counters {
	u.mu.RLock()
	defer u.mu.RUnlock()
	var out Counters
	for b, c := range u.buckets {
		if b.DeviceID == consumerDeviceID && b.EgressDeviceID != "" && b.Hour >= since.Truncate(time.Hour).Unix() {
			out.RxBytes += c.RxBytes
			out.TxBytes += c.TxBytes
		}
	}
	return out
}

// ConsumerRow 某消费者设备在时段内的单计用量(发送方视角, 与配额实时口径一致)。
type ConsumerRow struct {
	AccountID   string `json:"account_id"`
	DeviceID    string `json:"device_id"`
	RxBytes     int64  `json:"rx_bytes"`
	TxBytes     int64  `json:"tx_bytes"`          // 计费用量(所有转发包)
	EgressBytes int64  `json:"egress_used_bytes"` // 其中走出口节点的部分
}

// PerConsumer 每消费者设备在 since 之后的用量明细, 单遍遍历。
func (u *UsageStore) PerConsumer(since time.Time) []ConsumerRow {
	u.mu.RLock()
	defer u.mu.RUnlock()
	idx := map[string]int{}
	out := []ConsumerRow{}
	hh := since.Truncate(time.Hour).Unix()
	for b, c := range u.buckets {
		if b.Hour < hh {
			continue
		}
		k := b.AccountID + "|" + b.DeviceID
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, ConsumerRow{AccountID: b.AccountID, DeviceID: b.DeviceID})
			i = len(out) - 1
		}
		r := &out[i]
		r.RxBytes += c.RxBytes
		r.TxBytes += c.TxBytes
		if b.EgressDeviceID != "" {
			r.EgressBytes += c.TxBytes
		}
	}
	return out
}

// keyedBuckets 转字符串键快照供序列化(Bucket 结构体键无法作为 json 对象成员名)。
func (u *UsageStore) keyedBuckets() map[string]*Counters {
	out := make(map[string]*Counters, len(u.buckets))
	for b, c := range u.buckets {
		key := b.AccountID + "|" + b.DeviceID + "|" + b.EgressDeviceID + "|" + strconv.FormatInt(b.Hour, 10)
		out[key] = &Counters{RxBytes: c.RxBytes, TxBytes: c.TxBytes}
	}
	return out
}

// Save 落盘(管理面按需调用)。
func (u *UsageStore) Save() error {
	u.mu.RLock()
	data, err := json.MarshalIndent(u.keyedBuckets(), "", "  ")
	u.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.path)
}
