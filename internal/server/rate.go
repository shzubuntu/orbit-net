package server

import (
	"sync"
	"time"
)

// rateLimiter 极简内存滑动窗口限流(按 key 计数,过窗口重置)。
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string]*rw
}

type rw struct {
	start time.Time
	n     int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{hits: map[string]*rw{}}
}

// Allow 计数并判断是否允许。达到 limit 后继续调用返回 false(不拒绝但阻止进一步动作)。
func (l *rateLimiter) Allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.hits[key]
	if !ok || now.Sub(e.start) >= window {
		e = &rw{start: now}
		l.hits[key] = e
		if len(l.hits) > 10000 { // 惰性清理防膨胀
			l.prune(now, window)
		}
	}
	e.n++
	return e.n <= limit
}

func (l *rateLimiter) prune(now time.Time, window time.Duration) {
	for k, e := range l.hits {
		if now.Sub(e.start) >= window {
			delete(l.hits, k)
		}
	}
}
