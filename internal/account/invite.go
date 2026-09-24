package account

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Invite 邀请码(公网内测白名单入口)。
type Invite struct {
	Code      string    `json:"code"`
	AccountID string    `json:"account_id,omitempty"` // 已消费的账户
	CreatedAt time.Time `json:"created_at"`
}

// InviteStore invites.json 持久化。
type InviteStore struct {
	mu    sync.RWMutex
	path  string
	codes map[string]*Invite
}

// OpenInvites 打开邀请码库。
func OpenInvites(path string) (*InviteStore, error) {
	is := &InviteStore{path: path, codes: map[string]*Invite{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		var cs []*Invite
		if err := json.Unmarshal(raw, &cs); err != nil {
			return nil, err
		}
		for _, c := range cs {
			is.codes[c.Code] = c
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return is, nil
}

// Generate 生成 n 个未消费邀请码。
func (is *InviteStore) Generate(n int) ([]string, error) {
	is.mu.Lock()
	defer is.mu.Unlock()
	var out []string
	for i := 0; i < n; i++ {
		code := randInviteCode()
		is.codes[code] = &Invite{Code: code, CreatedAt: time.Now()}
		out = append(out, code)
	}
	if err := is.save(); err != nil {
		return nil, err
	}
	return out, nil
}

// Consume 消费邀请码并绑定账户。
func (is *InviteStore) Consume(code, accountID string) error {
	is.mu.Lock()
	defer is.mu.Unlock()
	c, ok := is.codes[code]
	if !ok {
		return errors.New("invalid invite code")
	}
	if c.AccountID != "" {
		return errors.New("invite code already used")
	}
	c.AccountID = accountID
	return is.save()
}

// List 全部邀请码。
func (is *InviteStore) List() []*Invite {
	is.mu.RLock()
	defer is.mu.RUnlock()
	out := make([]*Invite, 0, len(is.codes))
	for _, c := range is.codes {
		out = append(out, c)
	}
	return out
}

// Remove 吊销未消费邀请码(已消费码与账户绑定,不可作废,仅可观察)。
func (is *InviteStore) Remove(code string) error {
	is.mu.Lock()
	defer is.mu.Unlock()
	c, ok := is.codes[code]
	if !ok {
		return errors.New("invite code not found")
	}
	if c.AccountID != "" {
		return errors.New("invite code already consumed, cannot revoke")
	}
	delete(is.codes, code)
	return is.save()
}

// save 落盘(caller 必须已持有锁:Generate/Consume)。
func (is *InviteStore) save() error {
	var cs []*Invite
	for _, c := range is.codes {
		cs = append(cs, c)
	}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	tmp := is.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, is.path)
}

// randInviteCode 生成 12 字符随机邀请码。
func randInviteCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去易混字符
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "FALLBACK"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
