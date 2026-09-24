// Package rollinglog 滚动日志: 单个文件写到阈值后改名保留最近 N 份。
// 支持并发写(每次写一把锁 + 预裁)。
package rollinglog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Manager 滚动日志管理器。
type Manager struct {
	mu       sync.Mutex
	path     string
	maxBytes int
	keep     int
}

// Setup 初始化滚动日志; maxBytes/keep 为 0 时返回 nil(仅标准输出)。
func Setup(path string, maxBytes, keep int) (*Manager, error) {
	if path == "" {
		return nil, nil
	}
	if keep <= 0 {
		keep = 3
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024 * 1024
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Manager{path: path, maxBytes: maxBytes, keep: keep}, nil
}

// Write 写一行; 超阈值先轮转。
func (m *Manager) Write(b []byte) (int, error) {
	if m == nil {
		return len(b), nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := os.Stat(m.path)
	if err == nil && st.Size() >= int64(m.maxBytes) {
		m.rotate()
	}
	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(b)
}

func (m *Manager) rotate() {
	for i := m.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", m.path, i)
		to := fmt.Sprintf("%s.%d", m.path, i+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	_ = os.Rename(m.path, m.path+".1")
}
