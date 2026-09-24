package account

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// ErrNotFound 记录不存在。
var ErrNotFound = errors.New("not found")

// Store 账户/设备持久化(JSON 文件, M1 规模够用; 落到多区域再评估嵌入式 DB)。
type Store struct {
	mu       sync.RWMutex
	path     string
	accounts map[string]*Account
	devices  map[string]*Device
}

type fileData struct {
	Accounts []*Account `json:"accounts"`
	Devices  []*Device  `json:"devices"`
}

// Open 打开(不存在则初始化)账户库。
func Open(path string) (*Store, error) {
	s := &Store{path: path, accounts: map[string]*Account{}, devices: map[string]*Device{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var fd fileData
	if err := json.Unmarshal(raw, &fd); err != nil {
		return nil, err
	}
	for _, a := range fd.Accounts {
		s.accounts[a.ID] = a
	}
	for _, d := range fd.Devices {
		s.devices[d.ID] = d
	}
	return s, nil
}

// save 落盘(caller 必须已持有写锁)。
func (s *Store) save() error {
	data, err := json.MarshalIndent(fileData{
		Accounts: s.accountsSlice(),
		Devices:  s.devicesSlice(),
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// accountsSlice 内部辅助: caller 必须已持锁。
func (s *Store) accountsSlice() []*Account {
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out
}

func (s *Store) devicesSlice() []*Device {
	out := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	return out
}

// AddAccount 新增账户。
func (s *Store) AddAccount(a *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[a.ID] = a
	return s.save()
}

// ListAccounts 全部账户。
func (s *Store) ListAccounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accountsSlice()
}

// ListDevicesAll 全部设备(管理面用)。
func (s *Store) ListDevicesAll() []*Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devicesSlice()
}

// SetAccountTier 修改账户档位(计费档位切换, 对在线的该账户节点推送新配额需调用方负责)。
func (s *Store) SetAccountTier(accountID, tier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[accountID]
	if !ok {
		return ErrNotFound
	}
	a.Tier = tier
	return s.save()
}

// RemoveAccount 删除账户及其设备(注册失败回滚用)。
func (s *Store) RemoveAccount(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range s.devices {
		if d.AccountID == accountID {
			delete(s.devices, id)
		}
	}
	delete(s.accounts, accountID)
	return s.save()
}

// Account 按 ID 取账户。
func (s *Store) Account(id string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

// AddDevice 绑定设备。
func (s *Store) AddDevice(d *Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices[d.ID] = d
	return s.save()
}

// Device 按 ID 取设备。
func (s *Store) Device(id string) (*Device, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

// DevicesByAccount 某账户的设备列表。
func (s *Store) DevicesByAccount(accountID string) []*Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Device
	for _, d := range s.devices {
		if d.AccountID == accountID {
			out = append(out, d)
		}
	}
	return out
}

// RemoveDevice 撤销设备: 删除记录,其 token 即刻失效(再连接直接 ERR_BAD_TOKEN)。
func (s *Store) RemoveDevice(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[deviceID]; !ok {
		return ErrNotFound
	}
	delete(s.devices, deviceID)
	return s.save()
}

// SetDeviceEgress 设置设备的出口授权(enabled + ACL 白名单),空 ACL=仅本人账户。
func (s *Store) SetDeviceEgress(deviceID string, enabled bool, acl []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[deviceID]
	if !ok {
		return ErrNotFound
	}
	d.EgressEnabled = enabled
	d.EgressACL = append([]string(nil), acl...)
	return s.save()
}
