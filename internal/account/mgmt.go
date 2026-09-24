package account

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrBadToken 设备 token 无效/不存在。
var ErrBadToken = errors.New("bad device token")

// CreateAccount 创建账户及其首台设备(默认档位 free),明文 token 仅此一次返回。
func (s *Store) CreateAccount(name string) (*Account, *Device, string, error) {
	return s.CreateAccountWithTier(name, "free")
}

// CreateAccountWithTier 创建账户及其首台设备,指定计费档位。明文 token 仅此一次返回。
func (s *Store) CreateAccountWithTier(name, tier string) (*Account, *Device, string, error) {
	now := time.Now()
	id := "acc_" + randID()
	token, err := NewToken()
	if err != nil {
		return nil, nil, "", err
	}
	hash, err := HashToken(token)
	if err != nil {
		return nil, nil, "", err
	}
	a := &Account{ID: id, Name: name, Tier: tier, CreatedAt: now}
	d := &Device{
		ID:        "dev_" + randID(),
		AccountID: id,
		Name:      "main",
		TokenHash: hash,
		CreatedAt: now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[id] = a
	s.devices[d.ID] = d
	if err := s.save(); err != nil {
		return nil, nil, "", err
	}
	return a, d, token, nil
}

// MaxDevicesPerAccount 每账户设备上限(token 泄露防护: 限制被无限加装)。
const MaxDevicesPerAccount = 16

// AddDeviceForAccount 为已有账户绑定新设备。
func (s *Store) AddDeviceForAccount(accountID, name string) (*Device, string, error) {
	if name == "" {
		name = "device"
	}
	if _, err := s.Account(accountID); err != nil {
		return nil, "", fmt.Errorf("account: %w", err)
	}
	if len(s.DevicesByAccount(accountID)) >= MaxDevicesPerAccount {
		return nil, "", fmt.Errorf("device limit reached (%d)", MaxDevicesPerAccount)
	}
	token, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	hash, err := HashToken(token)
	if err != nil {
		return nil, "", err
	}
	d := &Device{ID: "dev_" + randID(), AccountID: accountID, Name: name, TokenHash: hash, CreatedAt: time.Now()}
	if err := s.AddDevice(d); err != nil {
		return nil, "", err
	}
	return d, token, nil
}

// VerifyDevice 校验设备 token;通过返回设备与所属账户。
func (s *Store) VerifyDevice(deviceID, token string) (*Device, *Account, error) {
	d, err := s.Device(deviceID)
	if err != nil {
		return nil, nil, ErrBadToken
	}
	if !VerifyToken(d.TokenHash, token) {
		return nil, nil, ErrBadToken
	}
	a, err := s.Account(d.AccountID)
	if err != nil {
		return nil, nil, fmt.Errorf("orphan device: %w", err)
	}
	return d, a, nil
}

// MarkSeen 刷新设备最近在线时间。
func (s *Store) MarkSeen(d *Device) {
	if d == nil {
		return
	}
	s.mu.Lock()
	d.LastSeen = time.Now()
	s.mu.Unlock()
}

// ErrName 显示名非法。
var ErrName = errors.New("invalid device name")

// RenameDevice 修改设备显示名(自助改名)。
func (s *Store) RenameDevice(deviceID, name string) (*Device, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: empty", ErrName)
	}
	if len(name) > 64 {
		return nil, fmt.Errorf("%w: too long", ErrName)
	}
	d, err := s.Device(deviceID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d.Name = name
	if err := s.save(); err != nil {
		return nil, err
	}
	return d, nil
}

// RotateToken 轮换设备 token: 旧 token 即刻失效,返回新明文(仅此一次展示)。
func (s *Store) RotateToken(deviceID string) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	hash, err := HashToken(token)
	if err != nil {
		return "", err
	}
	d, err := s.Device(deviceID)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d.TokenHash = hash
	if err := s.save(); err != nil {
		return "", err
	}
	return token, nil
}
